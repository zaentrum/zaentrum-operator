package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file restores the bundled Postgres from a dump of its backups.
//
// The annotation zaentrum.io/restore-request names the dump, optionally with
// "#" and anything after it, so that the same dump can be asked for again. A
// value status.backup.restore.request has not answered starts one restore:
//
//	Stopping   the render stops every client of the database — each
//	           Deployment with a session in it or writing through
//	           katalog-manager-api, z.replicas "db" — and suspends the backups;
//	           once none of their pods is left and no backup runs, the restore
//	           Job starts
//	Running    the chart's restore Job (templates/postgres-restore.yaml), as a
//	           Job of its own: it makes sure the dump is whole, waits for every
//	           session to leave, and recreates each database
//	Succeeded, Failed or Refused, read from the Job; the next render starts the
//	           clients again, and the realm Job runs again over the restored
//	           realm
//
// It is the operator's to orchestrate: a render applied every thirty seconds
// would start any client stopped by hand again. One restore at a time and none
// while the database is being copied onto its claim; a request made in the
// meantime waits. The Restore condition says where it is. While it is in
// flight the phase is Restoring, and neither a verification nor a realm run
// starts.

const condTypeRestore = "Restore"

// dumpRequest is a restore request: a dump's name, optionally "#" and more.
var dumpRequest = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z)(#.*)?$`)

// restoreStatus is status.backup.restore, or nil.
func restoreStatus(z *zaentrumv1alpha1.Zaentrum) *zaentrumv1alpha1.RestoreStatus {
	if z.Status.Backup == nil {
		return nil
	}
	return z.Status.Backup.Restore
}

// restoring says whether a restore is in flight, and of which dump.
func restoring(z *zaentrumv1alpha1.Zaentrum) (string, bool) {
	rs := restoreStatus(z)
	if rs == nil || (rs.Result != zaentrumv1alpha1.RestoreStopping && rs.Result != zaentrumv1alpha1.RestoreRunning) {
		return "", false
	}
	m := dumpRequest.FindStringSubmatch(rs.Request)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// setRestore writes status.backup.restore and the Restore condition.
func setRestore(z *zaentrumv1alpha1.Zaentrum, rs *zaentrumv1alpha1.RestoreStatus) {
	if z.Status.Backup == nil {
		z.Status.Backup = &zaentrumv1alpha1.BackupStatus{}
	}
	rs.Message = clip(rs.Message, maxVerifyMessage)
	z.Status.Backup.Restore = rs
	switch rs.Result {
	case zaentrumv1alpha1.RestoreSucceeded:
		setCondition(z, condTypeRestore, metav1.ConditionTrue, "Succeeded", rs.Message)
	case zaentrumv1alpha1.RestoreFailed:
		setCondition(z, condTypeRestore, metav1.ConditionFalse, "Failed", rs.Message)
	case zaentrumv1alpha1.RestoreRefused:
		setCondition(z, condTypeRestore, metav1.ConditionFalse, "Refused", rs.Message)
	default:
		setCondition(z, condTypeRestore, metav1.ConditionUnknown, string(rs.Result), rs.Message)
	}
}

// planRestore decides, before the render, which dump this pass restores, if
// any: the one in flight, else a new request that can start now. copying says
// the bundled Postgres is being copied onto its claim, which a restore waits
// for.
func (r *ZaentrumReconciler) planRestore(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, copying bool) string {
	if dump, ok := restoring(z); ok {
		return dump
	}
	request := strings.TrimSpace(z.GetAnnotations()[zaentrumv1alpha1.RestoreRequestAnnotation])
	if rs := restoreStatus(z); request == "" || (rs != nil && rs.Request == request) {
		return ""
	}
	now := r.now()
	refuse := func(msg string) string {
		setRestore(z, &zaentrumv1alpha1.RestoreStatus{Request: request, Result: zaentrumv1alpha1.RestoreRefused,
			StartedAt: &now, FinishedAt: &now, Message: "refused: " + msg + "; nothing was changed"})
		log.FromContext(ctx).Info("restore: refused", "request", request, "reason", msg)
		return ""
	}
	if z.Spec.Databases.Mode == "external" {
		return refuse("the databases are external, with no bundled Postgres to restore")
	}
	m := dumpRequest.FindStringSubmatch(request)
	if m == nil {
		return refuse(fmt.Sprintf("%q names no dump: a dump is named like 2026-10-04T00-00-05Z, as status.backup.dumps lists them", request))
	}
	if copying {
		setCondition(z, condTypeRestore, metav1.ConditionUnknown, "Waiting",
			"the restore of "+m[1]+" starts once the bundled Postgres has been copied onto its claim")
		return ""
	}
	log.FromContext(ctx).Info("restore: stopping the database's clients", "dump", m[1])
	setRestore(z, &zaentrumv1alpha1.RestoreStatus{Request: request, Result: zaentrumv1alpha1.RestoreStopping,
		StartedAt: &now, Message: "restoring " + m[1] + ": stopping the database's clients"})
	return m[1]
}

// stepRestore takes this pass's step of the restore in flight, after the
// platform was applied with the database's clients stopped. platform and hooks
// are the pass's render. It reports whether to come back soon.
func (r *ZaentrumReconciler) stepRestore(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform, hooks []*unstructured.Unstructured) bool {
	dump, ok := restoring(z)
	if !ok {
		return false
	}
	rs := restoreStatus(z).DeepCopy()
	end := func(result zaentrumv1alpha1.RestoreResult, msg string) bool {
		now := r.now()
		rs.Result, rs.FinishedAt, rs.Message = result, &now, msg
		setRestore(z, rs)
		log.FromContext(ctx).Info("restore: ended", "dump", dump, "result", string(result), "job", rs.Job)
		if result == zaentrumv1alpha1.RestoreSucceeded {
			// The restored realm holds what was imported or set when the dump
			// was made; the realm Job brings it in step again.
			runs, err := r.realmRuns(ctx, z)
			for i := range runs {
				if err == nil {
					_ = r.Delete(ctx, &runs[i], client.PropagationPolicy(metav1.DeletePropagationBackground))
				}
			}
		}
		return false
	}

	switch rs.Result {
	case zaentrumv1alpha1.RestoreStopping:
		if why := r.clientsUp(ctx, z, platform); why != "" {
			rs.Message = "restoring " + dump + ": stopping the database's clients; " + why
			setRestore(z, rs)
			return true
		}
		jobs, err := r.backupJobs(ctx, z)
		if err != nil {
			return true
		}
		for i := range jobs {
			if done, _ := jobFinished(&jobs[i]); !done {
				rs.Message = "restoring " + dump + ": waiting for the backup " + jobs[i].Name + " to end"
				setRestore(z, rs)
				return true
			}
		}
		claim := backupClaim(z)
		var pvc corev1.PersistentVolumeClaim
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: claim}, &pvc); apierrors.IsNotFound(err) {
			return end(zaentrumv1alpha1.RestoreRefused, "refused: there is no claim "+claim+" to restore from; nothing was changed")
		} else if err != nil {
			return true
		}
		hook := templates.RestoreJob(hooks)
		if hook == nil {
			return end(zaentrumv1alpha1.RestoreRefused, "refused: the platform chart renders no restore Job; nothing was changed")
		}
		if err := r.deleteRestoreJobs(ctx, z); err != nil {
			return true
		}
		job, err := r.restoreJob(z, hook)
		if err == nil {
			err = r.Create(ctx, job)
		}
		if err != nil {
			return end(zaentrumv1alpha1.RestoreFailed, "could not start the restore Job: "+err.Error())
		}
		log.FromContext(ctx).Info("restore: started", "dump", dump, "job", job.GetName())
		rs.Result, rs.Job = zaentrumv1alpha1.RestoreRunning, job.GetName()
		rs.Message = fmt.Sprintf("%s restores %s", job.GetName(), dump)
		setRestore(z, rs)
		return true

	default: // Running
		var job batchv1.Job
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: rs.Job}, &job)
		if apierrors.IsNotFound(err) {
			return end(zaentrumv1alpha1.RestoreFailed, fmt.Sprintf("the restore Job %s is gone before it was read: "+
				"the databases may be partly restored; ask for the restore again", rs.Job))
		}
		if err != nil {
			return true
		}
		pods, err := r.runPods(ctx, &job)
		if err != nil {
			return true
		}
		done, failed := jobFinished(&job)
		if !done {
			msg := fmt.Sprintf("%s restores %s", job.Name, dump)
			if hint := waitingHint(pods); hint != "" {
				msg += "; " + hint
			}
			rs.Message = msg
			setRestore(z, rs)
			return true
		}
		var term *corev1.ContainerStateTerminated
		for i := range pods {
			for _, cs := range pods[i].Status.ContainerStatuses {
				if cs.Name == templates.RestoreContainer && cs.State.Terminated != nil {
					term = cs.State.Terminated
				}
			}
		}
		msg := ""
		if term != nil {
			msg = strings.TrimSpace(term.Message)
		}
		switch {
		case failed == nil:
			return end(zaentrumv1alpha1.RestoreSucceeded, orElse(msg, "restored "+dump))
		case term != nil && term.ExitCode == templates.RestoreRefusedExitCode:
			return end(zaentrumv1alpha1.RestoreRefused, orElse(msg, "refused; nothing was changed"))
		default:
			why := msg
			if why == "" {
				why = strings.TrimSuffix(failed.Reason+": "+failed.Message, ": ")
			}
			return end(zaentrumv1alpha1.RestoreFailed, fmt.Sprintf("%s failed: %s", job.Name, why))
		}
	}
}

// clientsUp says which of the Deployments the render stopped for the restore
// still has a pod, or "" when none has.
func (r *ZaentrumReconciler) clientsUp(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured) string {
	var up []string
	for _, o := range platform {
		if o.GetKind() != "Deployment" {
			continue
		}
		if n, found, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas"); !found || asCount(n) != 0 {
			continue
		}
		labels, _, _ := unstructured.NestedStringMap(o.Object, "spec", "selector", "matchLabels")
		if len(labels) == 0 {
			continue
		}
		var pods corev1.PodList
		if err := r.reader().List(ctx, &pods, client.InNamespace(z.Namespace), client.MatchingLabels(labels)); err != nil {
			up = append(up, o.GetName())
			continue
		}
		if len(pods.Items) > 0 {
			up = append(up, fmt.Sprintf("%s (%d pods)", o.GetName(), len(pods.Items)))
		}
	}
	if len(up) == 0 {
		return ""
	}
	return "still up: " + strings.Join(up, ", ")
}

// asCount reads a replica count as the render left it.
func asCount(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1
}

func orElse(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// restoreJob turns the rendered hook into a restore's Job, as realmJob does a
// realm run's.
func (r *ZaentrumReconciler) restoreJob(z *zaentrumv1alpha1.Zaentrum, hook *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	job := hook.DeepCopy()
	job.SetName(templates.RestoreJobName + "-" + utilrand.String(verifyJobSuffix))
	job.SetNamespace(z.Namespace)
	job.SetResourceVersion("")
	annotations := map[string]string{}
	for k, v := range job.GetAnnotations() {
		if !strings.HasPrefix(k, "helm.sh/") {
			annotations[k] = v
		}
	}
	job.SetAnnotations(annotations)
	lbls := job.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelDatabase] = databaseRestore
	job.SetLabels(lbls)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// deleteRestoreJobs removes the restore Jobs this Zaentrum owns: at most the
// latest is kept, for its logs.
func (r *ZaentrumReconciler) deleteRestoreJobs(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) error {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelDatabase: databaseRestore}); err != nil {
		return err
	}
	for i := range jobs.Items {
		if !metav1.IsControlledBy(&jobs.Items[i], z) {
			continue
		}
		if err := r.Delete(ctx, &jobs.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
