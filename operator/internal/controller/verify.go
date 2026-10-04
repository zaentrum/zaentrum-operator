package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// This file implements the platform's self-test: status.verification.
//
// The chart ships its checks the way Helm says a chart should — as a test hook
// (templates/tests/verify.yaml), so `helm test` runs them on a plain install.
// The operator runs that same hook itself. It is never applied with the
// platform: every pass splits the test hooks off the render, and the operator
// starts the verification Job as a Job of its own, under a fresh name, after
// each update that leaves the platform Ready and whenever the
// zaentrum.io/verify-request annotation carries a value it has not answered.
//
// The Job's check container is `zae doctor`, outside-in against the public URL
// with a real sign-in. Its compact report is the container's termination
// message, so the verdict comes back through the pod's status: no log scraping,
// no sidecar, and nothing in the run that may write to the API.
//
// One run at a time, and nothing is dropped: an update or a request that comes
// while a run is in flight waits for it to end, and starts on the pass after the
// verdict is written. status.verification.request is the last request answered;
// only a run a request started moves it, so an annotation left in place never
// reads as a new request after an update.
//
// What this must never do: move the phase or the Ready condition, block or fail
// a reconcile, re-run a failed fingerprint by itself (a broken platform is not
// mended by asking again every thirty seconds — only a new update or a new
// request starts another run), or let the test account's password out. The
// password is made here, kept in a Secret, never logged, and scrubbed from
// everything this copies into status.

const (
	// verifyRequeueAfter polls a run in flight. A Job watch would make the
	// operator cache every Job in the cluster; one GET every 10s while a run
	// lasts costs less.
	verifyRequeueAfter = 10 * time.Second

	condTypeVerified = "Verified"

	// labelVerification marks what the operator creates for verification: the
	// Job of each run, and the test account's Secret.
	labelVerification   = "zaentrum.io/verification"
	verificationRun     = "run"
	verificationAccount = "account"

	// The Job carries what started it, for whoever reads it with kubectl, and
	// so that status can follow it from the Job alone (adopt).
	annotationVerifyTrigger     = "zaentrum.io/verify-trigger"
	annotationVerifyFingerprint = "zaentrum.io/verify-fingerprint"
	// annotationVerifyRequest is the request a request-triggered run answers;
	// annotationVerifyVersion is status.currentVersion when the run started.
	annotationVerifyRequest = "zaentrum.io/verify-request"
	annotationVerifyVersion = "zaentrum.io/verify-version"
	// annotationVerifyCheckerFallback marks a run that checks with zae:latest
	// because no zae image carries the platform's own tag; its value is that tag.
	annotationVerifyCheckerFallback = "zaentrum.io/verify-checker-fallback"

	// What status.verification holds at most.
	maxVerifyChecks  = 40
	maxVerifyDetail  = 200
	maxVerifyName    = 100
	maxVerifyMessage = 512

	// maxVerifyReport bounds the report the operator parses at all. The kubelet
	// keeps at most 4 KiB of a termination message, so anything near this is
	// not a report.
	maxVerifyReport = 64 << 10

	// verifyReportVersion is the report format ("v") this operator reads.
	verifyReportVersion = 1

	verifyPasswordLen = 32
	verifyJobSuffix   = 5

	// redacted stands in for the test account's password wherever a report or
	// an error message would have carried it.
	redacted = "[redacted]"
	// minRedactLen keeps a short admin-provided password from blanking every
	// occurrence of a common word; a generated one is far longer.
	minRedactLen = 8
)

// verificationEnabled is spec.verification.enabled, true unless set false.
func verificationEnabled(z *zaentrumv1alpha1.Zaentrum) bool {
	return z.Spec.Verification.Enabled == nil || *z.Spec.Verification.Enabled
}

// bundledIdentity reports whether the platform runs its own Keycloak — the
// realm the operator prepares the test account in.
func bundledIdentity(z *zaentrumv1alpha1.Zaentrum) bool {
	return z.Spec.Identity.Mode == "" || z.Spec.Identity.Mode == zaentrumv1alpha1.IdentityBundled
}

// reader reads straight from the API server where the manager provides one, so
// verification does not make the operator cache every Job and Pod.
func (r *ZaentrumReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ZaentrumReconciler) now() metav1.Time {
	if r.Now != nil {
		return metav1.NewTime(r.Now())
	}
	return metav1.Now()
}

// verify takes this pass's step of the self-test and writes
// status.verification and the Verified condition. ready is whether the pass
// found the platform Ready; platform and tests are the pass's render, split and
// pinned. It reports whether to come back soon: a run is in flight, or one is
// waiting to start.
//
// It returns no error: whatever goes wrong is the verification's result, never
// the reconcile's.
func (r *ZaentrumReconciler) verify(
	ctx context.Context, z *zaentrumv1alpha1.Zaentrum, version string,
	platform, tests []*unstructured.Unstructured, ready bool,
) bool {
	prev := z.Status.Verification
	request := strings.TrimSpace(z.GetAnnotations()[zaentrumv1alpha1.VerifyRequestAnnotation])

	if !verificationEnabled(z) {
		if prev != nil && prev.Result == zaentrumv1alpha1.VerificationRunning {
			if err := r.deleteRuns(ctx, z); err != nil {
				log.FromContext(ctx).Info("verification disabled; the running Job could not be removed",
					"error", err.Error())
			}
		}
		// No fingerprint: turning verification back on verifies the platform
		// as it then stands. The last answered request stays; one asked while
		// disabled is answered once verification is back.
		v := &zaentrumv1alpha1.VerificationStatus{
			Result:  zaentrumv1alpha1.VerificationSkipped,
			Request: answered(prev),
			Version: version,
			Message: "verification is disabled (spec.verification.enabled is false)",
		}
		z.Status.Verification = v
		setVerified(z, v)
		return false
	}

	v := prev.DeepCopy()
	wasRunning := v != nil && v.Result == zaentrumv1alpha1.VerificationRunning
	if wasRunning {
		r.collect(ctx, z, v)
	}

	fingerprint := platformFingerprint(platform)
	var trigger zaentrumv1alpha1.VerificationTrigger
	switch {
	case !ready:
		// Never against a platform that is still rolling out: the run would
		// report the rollout. An update or a request waits for Ready.
	case request != "" && request != answered(v):
		// A request also covers a platform that changed meanwhile: one run
		// answers both.
		trigger = zaentrumv1alpha1.VerificationTriggerRequest
	case v == nil || v.Fingerprint != fingerprint:
		trigger = zaentrumv1alpha1.VerificationTriggerUpdate
	}

	waiting := false
	switch {
	case trigger == "":
	case wasRunning:
		// One run at a time. The next one waits for this one to end and, if
		// it ended in this very pass, for its verdict to be written: it
		// starts on the next pass.
		waiting = true
	default:
		v = r.startRun(ctx, z, trigger, request, answered(v), fingerprint, version, tests)
	}

	z.Status.Verification = v
	if v == nil {
		return false
	}
	setVerified(z, v)
	return waiting || v.Result == zaentrumv1alpha1.VerificationRunning
}

// answered is the last request the operator answered.
func answered(v *zaentrumv1alpha1.VerificationStatus) string {
	if v == nil {
		return ""
	}
	return v.Request
}

// platformFingerprint identifies what the platform runs: the first 12 hex
// characters of the sha256 over the sorted "deployment/container=image" lines
// of every Deployment (init containers included), images as rendered after
// digest pinning. A replica count or a config value does not move it; an image
// does.
func platformFingerprint(objs []*unstructured.Unstructured) string {
	var lines []string
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		for _, key := range []string{"initContainers", "containers"} {
			containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", key)
			for _, c := range containers {
				cm, _ := c.(map[string]interface{})
				name, _ := cm["name"].(string)
				image, _ := cm["image"].(string)
				lines = append(lines, o.GetName()+"/"+name+"="+image)
			}
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// startRun replaces the previous run with a new one. A run that cannot start is
// an Error for this fingerprint and request, and is not retried by itself.
//
// Only a run a request started answers it; a run an update started keeps the
// last answered request (lastAnswered), so the annotation left in place is not
// taken for a new request.
func (r *ZaentrumReconciler) startRun(
	ctx context.Context, z *zaentrumv1alpha1.Zaentrum, trigger zaentrumv1alpha1.VerificationTrigger,
	request, lastAnswered, fingerprint, version string, tests []*unstructured.Unstructured,
) *zaentrumv1alpha1.VerificationStatus {
	now := r.now()
	v := &zaentrumv1alpha1.VerificationStatus{
		Result:      zaentrumv1alpha1.VerificationRunning,
		Trigger:     trigger,
		Request:     lastAnswered,
		Fingerprint: fingerprint,
		Version:     version,
		StartedAt:   &now,
	}
	if trigger == zaentrumv1alpha1.VerificationTriggerRequest {
		v.Request = request
	}
	failed := func(msg string) *zaentrumv1alpha1.VerificationStatus {
		v.Result = zaentrumv1alpha1.VerificationError
		v.FinishedAt = &now
		v.Message = clip(msg, maxVerifyMessage)
		log.FromContext(ctx).Info("verification could not start", "reason", v.Message)
		return v
	}

	// One run at a time, held against the API server rather than against the
	// status this pass started from. A reconcile that worked from a stale read
	// of the Zaentrum — its status not yet showing the run the pass before had
	// started — once replaced that run with a second one, and the first was then
	// reported lost while the second's result was never read. A run in flight
	// that status does not name is followed instead of replaced; the update or
	// request that would have started another waits for it, as any does.
	runs, err := r.runs(ctx, z)
	if err != nil {
		return failed("could not list the previous verification Jobs: " + err.Error())
	}
	for i := range runs {
		if done, _ := jobFinished(&runs[i]); !done {
			log.FromContext(ctx).Info("verification: a run is already in flight; following it instead of starting another",
				"job", runs[i].Name)
			return adopt(&runs[i], lastAnswered)
		}
	}
	// At most one run, ever: the previous Job goes before the next one comes.
	if err := r.deleteRuns(ctx, z); err != nil {
		return failed("could not remove the previous verification Job: " + err.Error())
	}
	hook := templates.VerifyJob(tests)
	if hook == nil {
		return failed("the platform chart renders no verification Job")
	}
	if err := r.ensureVerifyAccount(ctx, z); err != nil {
		return failed("could not prepare Secret " + templates.VerifySecretName + ": " + err.Error())
	}
	job, err := r.runJob(z, hook, v)
	if err != nil {
		return failed("could not prepare the verification Job: " + err.Error())
	}
	if err := r.Create(ctx, job); err != nil {
		return failed("could not start the verification Job: " + err.Error())
	}
	v.Job = job.GetName()
	log.FromContext(ctx).Info("verification started",
		"job", v.Job, "trigger", string(trigger), "fingerprint", fingerprint, "version", version)
	return v
}

// runJob turns the rendered hook into this run's Job: a name of its own, owned
// by the Zaentrum so it goes with the platform, marked as a run, and without
// the Helm hook annotations, which mean nothing outside a Helm release.
func (r *ZaentrumReconciler) runJob(
	z *zaentrumv1alpha1.Zaentrum, hook *unstructured.Unstructured, v *zaentrumv1alpha1.VerificationStatus,
) (*unstructured.Unstructured, error) {
	job := hook.DeepCopy()
	job.SetName(templates.VerifyJobName + "-" + utilrand.String(verifyJobSuffix))
	job.SetNamespace(z.Namespace)
	job.SetResourceVersion("")

	annotations := map[string]string{}
	for k, val := range job.GetAnnotations() {
		if !strings.HasPrefix(k, "helm.sh/") {
			annotations[k] = val
		}
	}
	annotations[annotationVerifyTrigger] = string(v.Trigger)
	annotations[annotationVerifyFingerprint] = v.Fingerprint
	annotations[annotationVerifyVersion] = v.Version
	if v.Trigger == zaentrumv1alpha1.VerificationTriggerRequest {
		annotations[annotationVerifyRequest] = v.Request
	}
	job.SetAnnotations(annotations)

	lbls := job.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelVerification] = verificationRun
	job.SetLabels(lbls)

	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// runs lists the verification runs this Zaentrum owns, read from the API
// server, newest first. A Job being deleted is no run any more.
func (r *ZaentrumReconciler) runs(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelVerification: verificationRun}); err != nil {
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

// adopt is the status of a run read from its Job alone: what started it, the
// platform it verifies and the request it answers, as runJob wrote them on it.
// lastAnswered is the last request answered before it, which a run an update
// started keeps.
func adopt(job *batchv1.Job, lastAnswered string) *zaentrumv1alpha1.VerificationStatus {
	v := &zaentrumv1alpha1.VerificationStatus{
		Result:      zaentrumv1alpha1.VerificationRunning,
		Request:     lastAnswered,
		Fingerprint: job.Annotations[annotationVerifyFingerprint],
		Version:     job.Annotations[annotationVerifyVersion],
		Job:         job.Name,
	}
	switch trigger := zaentrumv1alpha1.VerificationTrigger(job.Annotations[annotationVerifyTrigger]); trigger {
	case zaentrumv1alpha1.VerificationTriggerRequest:
		v.Trigger = trigger
		if q := job.Annotations[annotationVerifyRequest]; q != "" {
			v.Request = q
		}
	case zaentrumv1alpha1.VerificationTriggerUpdate:
		v.Trigger = trigger
	}
	if !job.CreationTimestamp.IsZero() {
		started := job.CreationTimestamp
		v.StartedAt = &started
	}
	return v
}

// deleteRuns removes every verification Job this Zaentrum owns, with background
// propagation: the Job goes now, its pod right after.
func (r *ZaentrumReconciler) deleteRuns(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) error {
	var jobs batchv1.JobList
	if err := r.reader().List(ctx, &jobs, client.InNamespace(z.Namespace),
		client.MatchingLabels{labelVerification: verificationRun}); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, z) {
			continue
		}
		err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// ── the test account ────────────────────────────────────────────────────────

// ensureVerifyAccount keeps Secret zaentrum-verify, the account the checks sign
// in with.
//
// With bundled identity the Secret is the operator's own state, made whether or
// not secrets.external is set: created once with the username and a crypto/rand
// password, owned by the Zaentrum, and never rotated — only a key that has gone
// missing is filled in. The run's init container then makes the realm agree.
//
// With an external identity provider the account is the admin's to provide: the
// operator creates nothing, and removes the Secret it generated for the bundled
// realm, if one is left from before, since that password signs in nowhere else.
func (r *ZaentrumReconciler) ensureVerifyAccount(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) error {
	key := types.NamespacedName{Namespace: z.Namespace, Name: templates.VerifySecretName}
	var sec corev1.Secret
	err := r.reader().Get(ctx, key, &sec)
	switch {
	case apierrors.IsNotFound(err):
		if !bundledIdentity(z) {
			return nil
		}
		password, perr := newVerifyPassword()
		if perr != nil {
			return perr
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: verifyAccountLabels(z)},
			Type:       corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte(templates.VerifyAccountName),
				"password": password,
			},
		}
		if err := controllerutil.SetControllerReference(z, &sec, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		log.FromContext(ctx).Info("verification: created the test account's Secret", "secret", key.Name)
		return nil
	case err != nil:
		return err
	}

	if !bundledIdentity(z) {
		if generatedAccount(z, &sec) {
			if err := r.Delete(ctx, &sec); client.IgnoreNotFound(err) != nil {
				return err
			}
			log.FromContext(ctx).Info("verification: removed the bundled realm's test account Secret", "secret", key.Name)
		}
		return nil
	}

	changed := false
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	if len(sec.Data["username"]) == 0 {
		sec.Data["username"] = []byte(templates.VerifyAccountName)
		changed = true
	}
	if len(sec.Data["password"]) == 0 {
		password, perr := newVerifyPassword()
		if perr != nil {
			return perr
		}
		sec.Data["password"] = password
		changed = true
	}
	if changed {
		return r.Update(ctx, &sec)
	}
	return nil
}

func verifyAccountLabels(z *zaentrumv1alpha1.Zaentrum) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      templates.VerifySecretName,
		"app.kubernetes.io/component": "verification",
		"app.kubernetes.io/part-of":   partOf(z),
		labelVerification:             verificationAccount,
	}
}

// partOf is the app.kubernetes.io/part-of value the chart labels the platform
// with: spec.partOf, else the namespace.
func partOf(z *zaentrumv1alpha1.Zaentrum) string {
	if z.Spec.PartOf != "" {
		return z.Spec.PartOf
	}
	return z.Namespace
}

// generatedAccount reports whether the operator made this Secret.
func generatedAccount(z *zaentrumv1alpha1.Zaentrum, sec *corev1.Secret) bool {
	return metav1.IsControlledBy(sec, z) && sec.Labels[labelVerification] == verificationAccount
}

// Every class a password policy may ask for is in a generated password.
var verifyPasswordClasses = []string{
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"abcdefghijklmnopqrstuvwxyz",
	"0123456789",
	"-_.~",
}

// newVerifyPassword makes a 32-character password from crypto/rand that
// satisfies the usual realm policies: an upper- and a lower-case letter, a digit
// and a symbol are each guaranteed, the rest drawn from all of them, then
// shuffled.
func newVerifyPassword() ([]byte, error) {
	return randomString(verifyPasswordClasses, verifyPasswordLen)
}

func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// redactor returns a function that blanks the test account's password out of
// text bound for status. The report should never carry it; this makes sure.
func (r *ZaentrumReconciler) redactor(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) func(string) string {
	var sec corev1.Secret
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: templates.VerifySecretName}, &sec)
	password := ""
	if err == nil {
		password = string(sec.Data["password"])
	}
	return func(s string) string {
		if len(password) < minRedactLen {
			return s
		}
		return strings.ReplaceAll(s, password, redacted)
	}
}

// ── the checker's image ─────────────────────────────────────────────────────

// imageResolver is the part of the digest resolver the checker's fallback asks.
type imageResolver interface {
	Resolve(ctx context.Context, image string) (string, error)
}

// checkerFallback keeps the platform's checks runnable on every platform
// version. The verification Job runs the zae image of the platform's own tag,
// as every component runs its image of that tag; a platform release cut
// without a zae image of the same tag would leave the run unable to start. So
// when the registry says the zae image has no such tag, the Job checks with
// zae:latest instead, and the run's message says so: the checks are
// outside-in and read nothing they cannot skip, so a newer checker suits an
// older platform. Any other registry failure keeps the tag — the registry was
// not answering, which says nothing about the tag. It returns what it did, or
// "" when it left the Job alone.
func checkerFallback(ctx context.Context, rv imageResolver, objs []*unstructured.Unstructured) string {
	job := templates.VerifyJob(objs)
	if job == nil {
		return ""
	}
	spec, _ := job.Object["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != templates.VerifyCheckContainer {
			continue
		}
		image, _ := cm["image"].(string)
		ref := digest.Parse(image)
		if ref.Registry != "ghcr.io" || ref.Repo != "zaentrum/zae" || ref.Pinned() || ref.Tag == "" || ref.Tag == "latest" {
			return ""
		}
		if _, err := rv.Resolve(ctx, image); err == nil || !errors.Is(err, digest.ErrNotFound) {
			return ""
		}
		cm["image"] = ref.Registry + "/" + ref.Repo + ":latest"
		annotations := job.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[annotationVerifyCheckerFallback] = ref.Tag
		job.SetAnnotations(annotations)
		return fmt.Sprintf("no zae image is tagged %s; the platform's checks run zae:latest", ref.Tag)
	}
	return ""
}

// ── the run's result ────────────────────────────────────────────────────────

// collect reads the in-flight run's Job and, once it has finished, the verdict.
// A read that fails is tried again next pass. A Job that is gone was replaced
// by the run that is there now, which is followed to its end; one that left no
// run behind is an Error.
func (r *ZaentrumReconciler) collect(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, v *zaentrumv1alpha1.VerificationStatus) {
	logger := log.FromContext(ctx)
	var job batchv1.Job
	var err error = apierrors.NewNotFound(batchv1.Resource("jobs"), v.Job)
	if v.Job != "" {
		err = r.reader().Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: v.Job}, &job)
	}
	if apierrors.IsNotFound(err) {
		runs, lerr := r.runs(ctx, z)
		if lerr != nil {
			logger.Info("verification: the runs could not be listed; trying again", "error", lerr.Error())
			return
		}
		if len(runs) == 0 {
			r.lost(v)
			return
		}
		// Only one run is ever kept, so the one there is the latest: a run
		// that replaced this one. Its own Job says what it verifies.
		logger.Info("verification: the run's Job is gone; following the run that replaced it",
			"gone", v.Job, "job", runs[0].Name)
		*v = *adopt(&runs[0], v.Request)
		job, err = runs[0], nil
	}
	if err != nil {
		logger.Info("verification: the run's Job could not be read; trying again", "job", v.Job, "error", err.Error())
		return
	}
	pods, err := r.runPods(ctx, &job)
	if err != nil {
		logger.Info("verification: the run's pod could not be read; trying again", "job", v.Job, "error", err.Error())
		return
	}

	done, failedCond := jobFinished(&job)
	if !done {
		// What holds the run up, while it is held up — so an image that never
		// pulls is visible long before the deadline turns it into an Error.
		v.Message = clip(waitingHint(pods), maxVerifyMessage)
		return
	}

	judge(v, runOutcomeOf(pods, failedCond), v.Message, r.redactor(ctx, z))
	if tag := job.GetAnnotations()[annotationVerifyCheckerFallback]; tag != "" {
		v.Message = clip(v.Message+"; checked with zae:latest, as no zae image is tagged "+tag, maxVerifyMessage)
	}
	if v.FinishedAt == nil {
		finished := r.now()
		v.FinishedAt = &finished
	}
	logger.Info("verification finished", "job", v.Job, "result", string(v.Result),
		"passed", v.Passed, "failed", v.Failed, "warned", v.Warned, "skipped", v.Skipped)
}

// lost ends a run whose Job is gone before its result was read.
func (r *ZaentrumReconciler) lost(v *zaentrumv1alpha1.VerificationStatus) {
	finished := r.now()
	v.Result = zaentrumv1alpha1.VerificationError
	v.FinishedAt = &finished
	v.Message = clip(fmt.Sprintf("the verification Job %q is gone; its result was never read", v.Job), maxVerifyMessage)
}

// runPods lists the pods of a run's Job.
func (r *ZaentrumReconciler) runPods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	selector := labels.SelectorFromSet(labels.Set{batchv1.JobNameLabel: job.Name})
	if job.Spec.Selector != nil {
		s, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
		if err != nil {
			return nil, err
		}
		selector = s
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// jobFinished reports whether the Job has ended and, if it failed, how.
func jobFinished(job *batchv1.Job) (bool, *batchv1.JobCondition) {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, nil
		case batchv1.JobFailed:
			return true, c
		}
	}
	return false, nil
}

// runOutcome is what a finished run left behind.
type runOutcome struct {
	// podFound is false when the Job's pod is gone (the deadline deletes it).
	podFound bool
	// ran is true when the check container ran to an end.
	ran      bool
	exitCode int32
	reason   string
	// report is the check container's termination message.
	report     string
	finishedAt *metav1.Time
	// accountFailure is why the init container could not prepare the account.
	accountFailure string
	// jobFailure is the Job's own failure, "Reason: message".
	jobFailure string
}

func runOutcomeOf(pods []corev1.Pod, failedCond *batchv1.JobCondition) runOutcome {
	var out runOutcome
	if failedCond != nil {
		out.jobFailure = strings.TrimSuffix(failedCond.Reason+": "+failedCond.Message, ": ")
	}
	pod := chooseRunPod(pods)
	if pod == nil {
		return out
	}
	out.podFound = true
	for _, cs := range pod.Status.InitContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			out.accountFailure = strings.TrimSpace(t.Message)
			if out.accountFailure == "" {
				out.accountFailure = fmt.Sprintf("the %s container exited %d (%s)", cs.Name, t.ExitCode, orUnknown(t.Reason))
			}
		}
	}
	if t := checkTerminated(pod); t != nil {
		out.ran = true
		out.exitCode = t.ExitCode
		out.reason = t.Reason
		out.report = t.Message
		if !t.FinishedAt.IsZero() {
			finished := t.FinishedAt
			out.finishedAt = &finished
		}
	}
	return out
}

// chooseRunPod picks the pod that holds the run's result: the one whose check
// container ended last, else any.
func chooseRunPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	var bestEnd *corev1.ContainerStateTerminated
	for i := range pods {
		end := checkTerminated(&pods[i])
		switch {
		case best == nil:
		case end == nil:
			continue
		case bestEnd != nil && !end.FinishedAt.After(bestEnd.FinishedAt.Time):
			continue
		}
		best, bestEnd = &pods[i], end
	}
	return best
}

func checkTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == templates.VerifyCheckContainer {
			return cs.State.Terminated
		}
	}
	return nil
}

// waitingHint names what keeps a run's pod from running — an image that does
// not pull, a Secret that is not there, a pod nothing schedules — or "".
func waitingHint(pods []corev1.Pod) string {
	for _, pod := range pods {
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...),
			pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w == nil || w.Reason == "" || w.Reason == "ContainerCreating" || w.Reason == "PodInitializing" {
				continue
			}
			return strings.TrimSuffix(fmt.Sprintf("the %s container is waiting: %s: %s", cs.Name, w.Reason, w.Message), ": ")
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
				return strings.TrimSuffix(fmt.Sprintf("the pod is not scheduled: %s: %s", c.Reason, c.Message), ": ")
			}
		}
	}
	return ""
}

// ── the report ──────────────────────────────────────────────────────────────

// doctorReport is the compact report `zae doctor --report` writes as the check
// container's termination message.
type doctorReport struct {
	V       int           `json:"v"`
	Zae     string        `json:"zae"`
	URL     string        `json:"url"`
	Passed  int32         `json:"passed"`
	Failed  int32         `json:"failed"`
	Warned  int32         `json:"warned"`
	Skipped int32         `json:"skipped"`
	Checks  []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	N string `json:"n"`
	S string `json:"s"`
	D string `json:"d"`
}

var errNoReport = errors.New("no report")

// parseReport reads the report strictly: an unknown format version, a negative
// count, or a check with no name or an unknown status makes it unreadable rather
// than half-read. A verdict people act on is not guessed from a report this
// operator does not understand.
func parseReport(raw string) (*doctorReport, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errNoReport
	}
	if len(raw) > maxVerifyReport {
		return nil, fmt.Errorf("the report is %d bytes, more than any report", len(raw))
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, fmt.Errorf("the report is not JSON (%v)", err)
	}
	if rep.V != verifyReportVersion {
		return nil, fmt.Errorf("the report is format %d; this operator reads format %d", rep.V, verifyReportVersion)
	}
	if rep.Passed < 0 || rep.Failed < 0 || rep.Warned < 0 || rep.Skipped < 0 {
		return nil, errors.New("the report has a negative count")
	}
	for i := range rep.Checks {
		c := &rep.Checks[i]
		c.N = strings.TrimSpace(c.N)
		if c.N == "" {
			return nil, fmt.Errorf("check %d of the report has no name", i+1)
		}
		switch s := zaentrumv1alpha1.VerificationCheckStatus(strings.ToLower(strings.TrimSpace(c.S))); s {
		case zaentrumv1alpha1.VerificationCheckOK, zaentrumv1alpha1.VerificationCheckWarn,
			zaentrumv1alpha1.VerificationCheckFail, zaentrumv1alpha1.VerificationCheckSkip:
			c.S = string(s)
		default:
			return nil, fmt.Errorf("check %q of the report has the status %q", clip(c.N, maxVerifyName), clip(c.S, 16))
		}
	}
	return &rep, nil
}

// judge turns a finished run into the verdict.
//
// A readable report decides pass or fail: any failed check is Failed whatever
// the exit code, and Passed takes exit 0 as well. A report and an exit code that
// disagree the other way — no failure listed, yet a non-zero exit, as when the
// runner is killed after writing its report — is an Error: no verdict. Without a
// readable report there is no verdict either, and message says what happened
// instead. lastSeen is what held the run up while it was in flight.
func judge(v *zaentrumv1alpha1.VerificationStatus, out runOutcome, lastSeen string, redact func(string) string) {
	if out.finishedAt != nil {
		v.FinishedAt = out.finishedAt
	}

	rep, perr := parseReport(out.report)
	if perr == nil {
		dropped := fillChecks(v, rep, redact)
		switch {
		case v.Failed > 0:
			v.Result = zaentrumv1alpha1.VerificationFailed
			v.Message = summary(v, rep, dropped)
		case out.ran && out.exitCode == 0:
			v.Result = zaentrumv1alpha1.VerificationPassed
			v.Message = summary(v, rep, dropped)
		default:
			v.Result = zaentrumv1alpha1.VerificationError
			v.Message = fmt.Sprintf("the check runner exited %d (%s), although its report lists no failed check",
				out.exitCode, orUnknown(out.reason))
		}
		v.Message = clip(redact(v.Message), maxVerifyMessage)
		return
	}

	v.Result = zaentrumv1alpha1.VerificationError
	var msg string
	switch {
	case out.accountFailure != "":
		msg = "the test account could not be prepared: " + out.accountFailure
	case out.ran && errors.Is(perr, errNoReport):
		msg = fmt.Sprintf("the check runner exited %d (%s) without a report", out.exitCode, orUnknown(out.reason))
	case out.ran:
		msg = fmt.Sprintf("the check runner exited %d (%s) and its report could not be read: %v",
			out.exitCode, orUnknown(out.reason), perr)
	case out.jobFailure != "":
		msg = "the run ended without a report: " + out.jobFailure
	case !out.podFound:
		msg = "the run ended, but its pod is gone and the report with it"
	default:
		msg = "the run ended without a report"
	}
	if !out.ran && lastSeen != "" {
		msg += "; last seen: " + lastSeen
	}
	v.Message = clip(redact(msg), maxVerifyMessage)
}

// fillChecks copies the report's counts and checks into status, capped, and
// returns how many checks it left out. The counts are the report's own — the
// runner may list fewer checks than it counted — unless they account for fewer
// checks than it lists, or say nothing failed while a listed check did; then
// they are counted from the list.
func fillChecks(v *zaentrumv1alpha1.VerificationStatus, rep *doctorReport, redact func(string) string) int {
	var listed [4]int32 // ok, warn, fail, skip
	for _, c := range rep.Checks {
		switch zaentrumv1alpha1.VerificationCheckStatus(c.S) {
		case zaentrumv1alpha1.VerificationCheckOK:
			listed[0]++
		case zaentrumv1alpha1.VerificationCheckWarn:
			listed[1]++
		case zaentrumv1alpha1.VerificationCheckFail:
			listed[2]++
		default:
			listed[3]++
		}
	}
	v.Passed, v.Warned, v.Failed, v.Skipped = rep.Passed, rep.Warned, rep.Failed, rep.Skipped
	if int(rep.Passed+rep.Warned+rep.Failed+rep.Skipped) < len(rep.Checks) || (rep.Failed == 0 && listed[2] > 0) {
		v.Passed, v.Warned, v.Failed, v.Skipped = listed[0], listed[1], listed[2], listed[3]
	}

	all := make([]zaentrumv1alpha1.VerificationCheck, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		all = append(all, zaentrumv1alpha1.VerificationCheck{
			Name:   clip(redact(c.N), maxVerifyName),
			Status: zaentrumv1alpha1.VerificationCheckStatus(c.S),
			Detail: clip(redact(c.D), maxVerifyDetail),
		})
	}
	kept, dropped := keepChecks(all, maxVerifyChecks)
	if len(kept) > 0 {
		v.Checks = kept
	}
	return dropped
}

// keepChecks keeps at most n checks in report order, failures first and
// warnings next, so a long report never hides what went wrong.
func keepChecks(all []zaentrumv1alpha1.VerificationCheck, n int) ([]zaentrumv1alpha1.VerificationCheck, int) {
	if len(all) <= n {
		return all, 0
	}
	rank := func(s zaentrumv1alpha1.VerificationCheckStatus) int {
		switch s {
		case zaentrumv1alpha1.VerificationCheckFail:
			return 0
		case zaentrumv1alpha1.VerificationCheckWarn:
			return 1
		}
		return 2
	}
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rank(all[order[a]].Status) < rank(all[order[b]].Status) })
	keep := order[:n]
	sort.Ints(keep)
	out := make([]zaentrumv1alpha1.VerificationCheck, 0, n)
	for _, i := range keep {
		out = append(out, all[i])
	}
	return out, len(all) - n
}

// summary is the one line about a verdict: how many checks did what, and the
// names behind every failure, warning and skip.
func summary(v *zaentrumv1alpha1.VerificationStatus, rep *doctorReport, dropped int) string {
	total := v.Passed + v.Failed + v.Warned + v.Skipped
	names := func(s zaentrumv1alpha1.VerificationCheckStatus) string {
		var out []string
		for _, c := range rep.Checks {
			if c.S == string(s) {
				out = append(out, c.N)
			}
		}
		return strings.Join(out, ", ")
	}
	var parts []string
	if v.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d checks failed", v.Failed, total))
		if n := names(zaentrumv1alpha1.VerificationCheckFail); n != "" {
			parts[0] += ": " + n
		}
	} else {
		parts = append(parts, fmt.Sprintf("%d of %d checks passed", v.Passed+v.Warned, total))
	}
	if n := names(zaentrumv1alpha1.VerificationCheckWarn); n != "" {
		parts = append(parts, "warnings: "+n)
	}
	if n := names(zaentrumv1alpha1.VerificationCheckSkip); n != "" {
		parts = append(parts, "skipped: "+n)
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d more checks not listed", dropped))
	}
	return strings.Join(parts, "; ")
}

// setVerified writes the Verified condition from the verification status.
func setVerified(z *zaentrumv1alpha1.Zaentrum, v *zaentrumv1alpha1.VerificationStatus) {
	switch v.Result {
	case zaentrumv1alpha1.VerificationPassed:
		setCondition(z, condTypeVerified, metav1.ConditionTrue, "Passed", v.Message)
	case zaentrumv1alpha1.VerificationFailed:
		setCondition(z, condTypeVerified, metav1.ConditionFalse, "Failed", v.Message)
	case zaentrumv1alpha1.VerificationError:
		setCondition(z, condTypeVerified, metav1.ConditionFalse, "Error", v.Message)
	case zaentrumv1alpha1.VerificationRunning:
		msg := fmt.Sprintf("verifying %s (%s) after an update", v.Version, v.Fingerprint)
		if v.Trigger == zaentrumv1alpha1.VerificationTriggerRequest {
			msg = fmt.Sprintf("verifying %s (%s) on request", v.Version, v.Fingerprint)
		}
		if v.Message != "" {
			msg += "; " + v.Message
		}
		setCondition(z, condTypeVerified, metav1.ConditionUnknown, "Running", msg)
	case zaentrumv1alpha1.VerificationSkipped:
		setCondition(z, condTypeVerified, metav1.ConditionUnknown, "Disabled", v.Message)
	}
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no reason given"
	}
	return s
}

// clip shortens s to at most n characters, marking the cut, and makes it valid
// UTF-8 (a termination message is bytes, and the kubelet may have cut it
// anywhere).
func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "\uFFFD")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
