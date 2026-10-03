package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// This file keeps the bundled realm in step with the platform.
//
// The realm import only ever makes a new realm, so what the chart decides
// about the realm — the redirect URIs, web origins and post-logout redirect
// URIs of the clients people sign in through — reaches a realm that exists
// only through the chart's realm Job (templates/realm.yaml). Helm runs it after
// every install and upgrade; the operator never applies it with the platform
// and runs it itself instead, as a Job of its own, whenever what it sets
// changes: the fingerprint of the rendered Job — its client list, its script
// and its image — differs from that of the last run. The Job compares before
// it writes, so a run over a realm that is already right changes nothing.
//
// One run at a time, and never beside a verification run: a realm run may move
// where the master realm signs in, which ends the master token the
// verification's account preparation holds. A run that succeeded is kept for
// a day (the hook's ttlSecondsAfterFinished) and then runs again, so a
// redirect someone added by hand does not stay; one that failed is tried again
// after realmRetryAfter, as a Keycloak that refused is often only a Keycloak
// that was restarting. A run starts only while Keycloak is available. The
// RealmConfigured condition says
// what the latest run did or why it failed. It never moves the phase or the
// Ready condition, and never fails a reconcile.

const (
	condTypeRealmConfigured = "RealmConfigured"

	// labelRealm marks the Jobs of realm runs.
	labelRealm = "zaentrum.io/realm"
	realmRun   = "config"

	annotationRealmFingerprint = "zaentrum.io/realm-fingerprint"

	// realmRetryAfter is how long a failed run waits before the next.
	realmRetryAfter = 10 * time.Minute

	keycloakDeployment = "keycloak"
)

// configureRealm takes this pass's step of keeping the bundled realm in step
// and writes the RealmConfigured condition. platform and hooks are the pass's
// render, split and pinned. It reports whether to come back soon: a run is in
// flight.
func (r *ZaentrumReconciler) configureRealm(ctx context.Context, z *zaentrumv1alpha1.Zaentrum,
	platform, hooks []*unstructured.Unstructured) bool {
	hook := templates.RealmJob(hooks)
	if hook == nil {
		// External identity: the chart renders no realm Job, as there is no
		// realm of the platform's to configure.
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeRealmConfigured)
		return false
	}
	logger := log.FromContext(ctx)
	fingerprint := realmFingerprint(hook)

	runs, err := r.realmRuns(ctx, z)
	if err != nil {
		logger.Info("realm: the runs could not be listed; trying again", "error", err.Error())
		return true
	}
	if len(runs) > 0 {
		latest := &runs[0]
		done, failed := jobFinished(latest)
		switch {
		case !done:
			// One at a time: a change waits for the run in flight to end.
			msg := fmt.Sprintf("%s brings the realm's clients in step", latest.Name)
			if pods, err := r.runPods(ctx, latest); err == nil {
				if hint := waitingHint(pods); hint != "" {
					msg += "; " + hint
				}
			}
			setCondition(z, condTypeRealmConfigured, metav1.ConditionUnknown, "Configuring", clip(msg, maxVerifyMessage))
			return true
		case latest.Annotations[annotationRealmFingerprint] != fingerprint:
			// The platform changed what the realm should hold: run again.
		case failed == nil:
			setCondition(z, condTypeRealmConfigured, metav1.ConditionTrue, "Configured",
				clip(r.realmReport(ctx, latest, "the realm's clients are in step"), maxVerifyMessage))
			return false
		default:
			ended := finishedAt(latest, failed)
			if retry := ended.Add(realmRetryAfter); r.now().Time.Before(retry) {
				setCondition(z, condTypeRealmConfigured, metav1.ConditionFalse, "Failed",
					clip(fmt.Sprintf("%s failed: %s; it runs again after %s", latest.Name,
						r.realmReport(ctx, latest, "no reason given"), retry.UTC().Format(time.RFC3339)), maxVerifyMessage))
				return false
			}
		}
	}

	if ready, why := keycloakAvailable(r.keycloak(ctx, z, platform)); !ready {
		setCondition(z, condTypeRealmConfigured, metav1.ConditionUnknown, "WaitingForKeycloak",
			"the realm's clients are brought in step once Keycloak is available: "+why)
		return false
	}
	if v := z.Status.Verification; v != nil && v.Result == zaentrumv1alpha1.VerificationRunning {
		// A run can move where the master realm signs in, which ends every
		// master token in flight — the one the verification's account
		// preparation holds among them. The two never overlap.
		setCondition(z, condTypeRealmConfigured, metav1.ConditionUnknown, "Waiting",
			"the realm's clients are brought in step once the verification run "+v.Job+" has ended")
		return true
	}
	for i := range runs {
		err := r.Delete(ctx, &runs[i], client.PropagationPolicy(metav1.DeletePropagationBackground))
		if client.IgnoreNotFound(err) != nil {
			logger.Info("realm: the previous run could not be removed; trying again", "job", runs[i].Name, "error", err.Error())
			return true
		}
	}
	job, err := r.realmJob(z, hook, fingerprint)
	if err == nil {
		err = r.Create(ctx, job)
	}
	if err != nil {
		setCondition(z, condTypeRealmConfigured, metav1.ConditionFalse, "Failed",
			clip("could not start the realm Job: "+err.Error(), maxVerifyMessage))
		return true
	}
	logger.Info("realm: started a run", "job", job.GetName(), "fingerprint", fingerprint)
	setCondition(z, condTypeRealmConfigured, metav1.ConditionUnknown, "Configuring",
		fmt.Sprintf("%s brings the realm's clients in step", job.GetName()))
	return true
}

// realmFingerprint identifies what a realm run sets: the first 12 hex
// characters of the sha256 over the rendered Job's pod template.
func realmFingerprint(hook *unstructured.Unstructured) string {
	tmpl, _, _ := unstructured.NestedMap(hook.Object, "spec", "template")
	b, _ := json.Marshal(tmpl)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// realmJob turns the rendered hook into a run's Job: a name of its own, owned
// by the Zaentrum, marked as a run of this fingerprint, and without the Helm
// hook annotations, which mean nothing outside a Helm release.
func (r *ZaentrumReconciler) realmJob(z *zaentrumv1alpha1.Zaentrum, hook *unstructured.Unstructured, fingerprint string) (*unstructured.Unstructured, error) {
	job := hook.DeepCopy()
	job.SetName(templates.RealmJobName + "-" + utilrand.String(verifyJobSuffix))
	job.SetNamespace(z.Namespace)
	job.SetResourceVersion("")
	annotations := map[string]string{}
	for k, v := range job.GetAnnotations() {
		if !strings.HasPrefix(k, "helm.sh/") {
			annotations[k] = v
		}
	}
	annotations[annotationRealmFingerprint] = fingerprint
	job.SetAnnotations(annotations)
	lbls := job.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelRealm] = realmRun
	job.SetLabels(lbls)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// realmRuns lists the realm runs this Zaentrum owns, newest first.
func (r *ZaentrumReconciler) realmRuns(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelRealm: realmRun}); err != nil {
		return nil, err
	}
	var out []batchv1.Job
	for _, j := range jobs.Items {
		if metav1.IsControlledBy(&j, z) && j.DeletionTimestamp == nil {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[b].CreationTimestamp.Before(&out[a].CreationTimestamp)
	})
	return out, nil
}

// realmReport is the run's termination message — its summary, or why it
// failed — else fallback.
func (r *ZaentrumReconciler) realmReport(ctx context.Context, job *batchv1.Job, fallback string) string {
	pods, err := r.runPods(ctx, job)
	if err != nil {
		return fallback
	}
	for i := range pods {
		if t := realmPodTerminated(&pods[i]); t != nil {
			if msg := strings.TrimSpace(t.Message); msg != "" {
				return msg
			}
		}
	}
	return fallback
}

// finishedAt is when a finished Job ended: its terminal condition's time.
func finishedAt(job *batchv1.Job, cond *batchv1.JobCondition) time.Time {
	if cond != nil && !cond.LastTransitionTime.IsZero() {
		return cond.LastTransitionTime.Time
	}
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	return job.CreationTimestamp.Time
}

// keycloak reads the platform's Keycloak Deployment, or nil when the render
// has none or it cannot be read.
func (r *ZaentrumReconciler) keycloak(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, platform []*unstructured.Unstructured) *appsv1.Deployment {
	for _, o := range platform {
		if o.GetKind() != "Deployment" || o.GetName() != keycloakDeployment {
			continue
		}
		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: keycloakDeployment}, &dep); err != nil {
			return nil
		}
		return &dep
	}
	return nil
}

// keycloakAvailable says whether Keycloak answers, with every replica of its
// current spec available; else why not.
func keycloakAvailable(dep *appsv1.Deployment) (bool, string) {
	if dep == nil {
		return false, "its Deployment is not there yet"
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	switch {
	case desired < 1:
		return false, "it is scaled to 0"
	case dep.Status.ObservedGeneration < dep.Generation || dep.Status.UpdatedReplicas < desired:
		return false, "a new Keycloak is rolling out"
	case dep.Status.AvailableReplicas < desired:
		return false, fmt.Sprintf("%d of %d replicas available", dep.Status.AvailableReplicas, desired)
	}
	return true, ""
}

// realmPodTerminated is the realm container's terminated state in pod, if any.
func realmPodTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == templates.RealmContainer {
			return cs.State.Terminated
		}
	}
	return nil
}
