package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file starts a backup on request.
//
// The annotation zaentrum.io/backup-request on the Zaentrum, with a value —
// trimmed, not empty — that has no answer yet, asks for one backup now. A
// value has its answer when status.backup.request holds it, or when an ended
// Job of the platform's carries it, so that a status that cannot hold the
// field (a CRD older than the operator) neither holds the schedule for good
// nor starts a second backup.
//
// The schedule stands still (backup.suspend, the CronJob's suspend) from the
// pass that takes a request up until its answer, and for as long as a backup
// asked for runs, whatever has become of its request, so that the schedule's
// run and the one asked for never write at once (backupScheduleHeld). The
// backup starts on a later pass: one that found the CronJob suspended before
// it applied anything, and finds no backup Job unfinished and no restore that
// stops the platform or has it stopped. A request waits for all of these; it
// is never refused because a backup runs.
//
// The backup is a Job of its own, made from the rendered CronJob's job
// template — the same script, mounts and label, so that whatever lists the
// backups' Jobs lists it too — named zaentrum-backup- and five random
// characters, owned by the Zaentrum, and annotated with the request's value:
// an operator that starts again finds which Job a request started, and only
// that Job answers it, never a backup that began before. Turning backups off
// removes the CronJob and its own Jobs, never this one.
//
// reportBackups (backup.go) answers the request: once the Job it started has
// ended and has been read; at once where the platform makes no backups; or,
// for a Job this operator started that is gone before it was read, as not
// read, with no second backup. It keeps the newest Job asked for that has
// ended, and deletes the ones before it once that one has been read. While
// the resource is refused (refusal.go) no pass renders, and a request waits
// until the resource is corrected.
//
// The backups' Jobs are watched (SetupWithManager), the CronJob's own as well
// as the ones asked for, so that a run's start and its end each bring a pass:
// status.backup.running follows them. The manager's cache holds those Jobs and
// no other (CacheOptions).

// maxLoggedRequest is how much of a request's value the log shows.
const maxLoggedRequest = 64

// askedBackup is what this operator remembers of a platform's backups asked
// for, between its passes: the Job it started for a request and has not yet
// read, which tells a Job gone before it was read from one never started; the
// value it answered last, which tells a status that did not keep the answer
// from a request with no answer; and whether it said so.
type askedBackup struct {
	request, job string
	answered     string
	toldUnkept   bool
}

// asked is what this operator remembers of z's backups asked for.
func (r *ZaentrumReconciler) asked(z *zaentrumv1alpha1.Zaentrum) askedBackup {
	r.askedMu.Lock()
	defer r.askedMu.Unlock()
	if a := r.askedBackups[z.UID]; a != nil {
		return *a
	}
	return askedBackup{}
}

// remember changes what this operator remembers of z's backups asked for.
func (r *ZaentrumReconciler) remember(z *zaentrumv1alpha1.Zaentrum, change func(*askedBackup)) {
	r.askedMu.Lock()
	defer r.askedMu.Unlock()
	if r.askedBackups == nil {
		r.askedBackups = map[types.UID]*askedBackup{}
	}
	a := r.askedBackups[z.UID]
	if a == nil {
		a = &askedBackup{}
		r.askedBackups[z.UID] = a
	}
	change(a)
}

// backupAsk is the backup the resource asks for, and the hold on the
// schedule, as a pass finds them before it renders.
type backupAsk struct {
	// request is the value with no answer yet, or "".
	request string
	// hold says that the render suspends the backups' CronJob.
	hold bool
	// suspended says that the backups' CronJob was suspended before the pass
	// applied it.
	suspended bool
}

// askedValue is the value of zaentrum.io/backup-request, trimmed.
func askedValue(z *zaentrumv1alpha1.Zaentrum) string {
	return strings.TrimSpace(z.GetAnnotations()[zaentrumv1alpha1.BackupRequestAnnotation])
}

// backupRequest is the backup the resource asks for that status.backup.request
// has not answered, or "".
func backupRequest(z *zaentrumv1alpha1.Zaentrum) string {
	request := askedValue(z)
	if request == "" || (z.Status.Backup != nil && z.Status.Backup.Request == request) {
		return ""
	}
	return request
}

// askedJob is the newest Job asked for of the platform's that carries the
// value, or nil. jobs are the backups' Jobs, newest first.
func askedJob(z *zaentrumv1alpha1.Zaentrum, jobs []batchv1.Job, value string) *batchv1.Job {
	for i := range jobs {
		if askedFor(z, &jobs[i]) && jobs[i].Annotations[zaentrumv1alpha1.BackupRequestAnnotation] == value {
			return &jobs[i]
		}
	}
	return nil
}

// askedFor says whether a backup Job is one asked for that the platform owns.
func askedFor(z *zaentrumv1alpha1.Zaentrum, job *batchv1.Job) bool {
	return job.Annotations[zaentrumv1alpha1.BackupRequestAnnotation] != "" && metav1.IsControlledBy(job, z)
}

// unanswered is the backup the resource asks for that has no answer yet, or
// "": status.backup.request does not hold it, no ended Job of the platform's
// carries it, and this operator did not answer it already.
func (r *ZaentrumReconciler) unanswered(z *zaentrumv1alpha1.Zaentrum, jobs []batchv1.Job) string {
	request := backupRequest(z)
	if request == "" || r.asked(z).answered == request {
		return ""
	}
	if job := askedJob(z, jobs, request); job != nil {
		if done, _ := jobFinished(job); done {
			return ""
		}
	}
	return request
}

// backupScheduleHeld says whether the backups' schedule stands still this
// pass: a backup asked for has no answer yet (request), a Job asked for of the
// platform's has not ended, whatever has become of its request, or the
// backups' Jobs could not be listed (err). jobs are the backups' Jobs. It is
// the one hold on the schedule; whatever else makes it stand still adds its
// reason here.
func backupScheduleHeld(z *zaentrumv1alpha1.Zaentrum, request string, jobs []batchv1.Job, err error) bool {
	if err != nil || request != "" {
		return true
	}
	for i := range jobs {
		if done, _ := jobFinished(&jobs[i]); !done && askedFor(z, &jobs[i]) {
			return true
		}
	}
	return false
}

// planBackupRequest reads, before the render, the backup the resource asks for
// that has no answer yet, whether the schedule stands still, and, for a
// request, whether the backups' CronJob is suspended as the pass finds it.
func (r *ZaentrumReconciler) planBackupRequest(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) backupAsk {
	jobs, err := r.backupJobs(ctx, z)
	ask := backupAsk{request: backupRequest(z)}
	if err == nil {
		ask.request = r.unanswered(z, jobs)
	}
	ask.hold = backupScheduleHeld(z, ask.request, jobs, err)
	if ask.request == "" {
		return ask
	}
	var cron batchv1.CronJob
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.BackupCronJobName}, &cron); err == nil {
		ask.suspended = cron.Spec.Suspend != nil && *cron.Spec.Suspend
	}
	return ask
}

// startBackupRequest starts the backup asked for, after the platform was
// applied with the schedule held still, once nothing stands in its way.
// platform is the pass's render; restoreDump is the dump a restore restores
// this pass, which stops the platform, or "". Where the platform makes no
// backups it starts nothing: reportBackups refuses the request. It reports
// whether the request waits to start, so that the next pass comes soon.
func (r *ZaentrumReconciler) startBackupRequest(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	platform []*unstructured.Unstructured, ask backupAsk, restoreDump string) bool {
	if ask.request == "" {
		return false
	}
	cron := renderedObject(platform, "CronJob", templates.BackupCronJobName)
	if cron == nil {
		return false
	}
	logger := log.FromContext(ctx)
	value := loggedRequest(ask.request)
	jobs, err := r.backupJobs(ctx, z)
	if err != nil {
		logger.Info("backups: the backup asked for waits", "request", value, "for", "the backups' Jobs to be listed", "error", err.Error())
		return true
	}
	if askedJob(z, jobs, ask.request) != nil {
		// Started: reportBackups answers it once its Job has ended.
		return false
	}
	if a := r.asked(z); a.request == ask.request {
		// Started by this operator, and gone before it was read:
		// reportBackups answers it as not read.
		return false
	}
	waits := ""
	switch running := runningBackup(jobs); {
	case restoreDump != "":
		waits = "the restore of " + restoreDump
	case running != "":
		waits = running
	case !ask.suspended:
		waits = "the schedule to stand still"
	}
	if waits != "" {
		logger.Info("backups: the backup asked for waits", "request", value, "for", waits)
		return true
	}

	job, err := r.requestedBackupJob(z, cron, ask.request)
	if err == nil {
		err = r.Create(ctx, job)
	}
	if err != nil {
		logger.Info("backups: the backup asked for waits", "request", value, "for", "its Job to be created", "error", err.Error())
		return true
	}
	r.remember(z, func(a *askedBackup) { a.request, a.job = ask.request, job.GetName() })
	logger.Info("backups: started a backup on request", "request", value, "job", job.GetName())
	return false
}

// answerBackup answers the backup asked for, value, in b, and logs why: msg
// with its keys. One this operator answered before, whose answer the status
// the pass read does not hold, is answered again without a word: the status
// cannot keep the field, and the log says so once.
func (r *ZaentrumReconciler) answerBackup(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, b *zaentrumv1alpha1.BackupStatus,
	value, msg string, kv ...any) {
	b.Request = value
	logger := log.FromContext(ctx)
	a := r.asked(z)
	if a.answered == value {
		if !a.toldUnkept {
			logger.Info("backups: the status did not keep the answer to the backup asked for: the resource's CRD may not serve status.backup.request",
				"request", loggedRequest(value))
			r.remember(z, func(a *askedBackup) { a.toldUnkept = true })
		}
		return
	}
	logger.Info(msg, append([]any{"request", loggedRequest(value)}, kv...)...)
	r.remember(z, func(a *askedBackup) {
		a.answered = value
		if a.request == value {
			a.request, a.job = "", ""
		}
	})
}

// loggedRequest is a request's value as the log shows it: its first
// maxLoggedRequest characters.
func loggedRequest(value string) string {
	return clip(value, maxLoggedRequest)
}

// renderedObject is the object of that kind and name in the render, or nil.
func renderedObject(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

// runningBackup is the name of the newest of the backups' Jobs that has not
// ended, or "". jobs are newest first.
func runningBackup(jobs []batchv1.Job) string {
	for i := range jobs {
		if done, _ := jobFinished(&jobs[i]); !done {
			return jobs[i].Name
		}
	}
	return ""
}

// requestedBackupJob is the Job of a backup asked for: the rendered CronJob's
// job template made into a Job, as `kubectl create job --from=cronjob/…`
// makes one, but owned by the Zaentrum, under a name of its own, and annotated
// with the request's value.
func (r *ZaentrumReconciler) requestedBackupJob(z *zaentrumv1alpha1.Zaentrum, cron *unstructured.Unstructured, request string) (*unstructured.Unstructured, error) {
	spec, found, err := unstructured.NestedMap(cron.Object, "spec", "jobTemplate", "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("the CronJob %s renders no job template", cron.GetName())
	}
	job := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	job.SetAPIVersion("batch/v1")
	job.SetKind("Job")
	job.SetName(templates.BackupCronJobName + "-" + utilrand.String(verifyJobSuffix))
	job.SetNamespace(z.Namespace)
	lbls, _, _ := unstructured.NestedStringMap(cron.Object, "spec", "jobTemplate", "metadata", "labels")
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelDatabase] = databaseBackup
	job.SetLabels(lbls)
	annotations, _, _ := unstructured.NestedStringMap(cron.Object, "spec", "jobTemplate", "metadata", "annotations")
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[zaentrumv1alpha1.BackupRequestAnnotation] = request
	job.SetAnnotations(annotations)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// keepNewestAskedFor deletes the Jobs asked for of the platform's that are
// older than the newest one that has ended, once the pass has read the
// backups' Jobs: that one has been read, and so have those before it. jobs are
// the backups' Jobs, newest first.
func (r *ZaentrumReconciler) keepNewestAskedFor(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, jobs []batchv1.Job) {
	kept := false
	for i := range jobs {
		job := &jobs[i]
		if !askedFor(z, job) {
			continue
		}
		done, _ := jobFinished(job)
		switch {
		case !done:
		case !kept:
			kept = true
		default:
			err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			if client.IgnoreNotFound(err) != nil {
				log.FromContext(ctx).Info("backups: a Job asked for before could not be removed; trying again", "job", job.Name, "error", err.Error())
			}
		}
	}
}

// backupRunEndedWhileRefused clears status.backup.running, on a pass that
// refuses the resource and reads nothing else, once the Job it names has ended
// or is gone.
func (r *ZaentrumReconciler) backupRunEndedWhileRefused(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) {
	b := z.Status.Backup
	if b == nil || b.Running == "" {
		return
	}
	var job batchv1.Job
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: b.Running}, &job)
	if done, _ := jobFinished(&job); apierrors.IsNotFound(err) || (err == nil && done) {
		b.Running = ""
	}
}

// backupJobSelector selects the backups' Jobs: the CronJob's own and the ones
// asked for.
func backupJobSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{labelDatabase: databaseBackup})
}

// CacheOptions are the manager's cache options the platform's reconciler
// needs: of the Jobs, the cache holds the backups' alone, which the reconciler
// watches. It reads every Job it acts on from the API server.
func CacheOptions() cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&batchv1.Job{}: {Label: backupJobSelector()},
	}}
}

// platformsOfBackupJob maps an event of one of the backups' Jobs to the
// platform it backs up: the Zaentrum in the Job's namespace. A run of the
// CronJob is the CronJob's own, one asked for the Zaentrum's.
func (r *ZaentrumReconciler) platformsOfBackupJob(ctx context.Context, obj client.Object) []reconcile.Request {
	if !backupJobSelector().Matches(labels.Set(obj.GetLabels())) {
		return nil
	}
	var list zaentrumv1alpha1.ZaentrumList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, z := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: z.Namespace, Name: z.Name}})
	}
	return reqs
}
