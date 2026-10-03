package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

const verifyNS = "zaentrum"

func verifyCR() *zaentrumv1alpha1.Zaentrum {
	z := &zaentrumv1alpha1.Zaentrum{ObjectMeta: metav1.ObjectMeta{
		Name: "zaentrum", Namespace: verifyNS, UID: "z-uid", Generation: 1,
	}}
	z.Spec.Features.Kafka = true
	z.Spec.Hostname = "media.example.org"
	return z
}

// verifyEnv is one platform and the verification passes taken over it, with a
// fake API server under them and a clock the test moves.
type verifyEnv struct {
	t        *testing.T
	c        client.WithWatch
	r        *ZaentrumReconciler
	z        *zaentrumv1alpha1.Zaentrum
	platform []*unstructured.Unstructured
	tests    []*unstructured.Unstructured
	clock    time.Time
}

func newVerifyEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, funcs *interceptor.Funcs, objs ...client.Object) *verifyEnv {
	t.Helper()
	s := selfScheme(t)
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	e := &verifyEnv{t: t, c: b.Build(), z: z, clock: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)}
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	e.render()
	return e
}

// render renders the platform as the reconciler does: split, test hooks apart.
func (e *verifyEnv) render() {
	e.t.Helper()
	objs, err := templates.Render(templates.NewValues(e.z))
	require.NoError(e.t, err)
	e.platform, e.tests = templates.SplitTestHooks(objs)
}

// pass takes one verification step, as one reconcile would.
func (e *verifyEnv) pass(ready bool) bool {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	return e.r.verify(context.Background(), e.z, "latest", e.platform, e.tests, ready)
}

func (e *verifyEnv) v() *zaentrumv1alpha1.VerificationStatus {
	e.t.Helper()
	require.NotNil(e.t, e.z.Status.Verification)
	return e.z.Status.Verification
}

func (e *verifyEnv) jobs() []batchv1.Job {
	e.t.Helper()
	var list batchv1.JobList
	require.NoError(e.t, e.c.List(context.Background(), &list, client.InNamespace(verifyNS)))
	return list.Items
}

func (e *verifyEnv) job(name string) *batchv1.Job {
	e.t.Helper()
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: name}, &job))
	return &job
}

func (e *verifyEnv) secret() *corev1.Secret {
	e.t.Helper()
	var sec corev1.Secret
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: templates.VerifySecretName}, &sec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(e.t, err)
	return &sec
}

// setImage moves one platform Deployment's image: an update.
func (e *verifyEnv) setImage(deployment, image string) {
	e.t.Helper()
	for _, o := range e.platform {
		if o.GetKind() == "Deployment" && o.GetName() == deployment {
			containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
			containers[0].(map[string]interface{})["image"] = image
			require.NoError(e.t, unstructured.SetNestedSlice(o.Object, containers, "spec", "template", "spec", "containers"))
			return
		}
	}
	e.t.Fatalf("no Deployment %s", deployment)
}

func (e *verifyEnv) request(token string) {
	ann := e.z.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[zaentrumv1alpha1.VerifyRequestAnnotation] = token
	e.z.SetAnnotations(ann)
}

// end finishes the current run's Job the way the Job controller and the kubelet
// do: a terminal condition on the Job, and its pod with the check container's
// terminated state — exit code, reason and termination message.
func (e *verifyEnv) end(exit int32, reason, report string) {
	e.t.Helper()
	job := e.job(e.v().Job)
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	if exit != 0 {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}
	}
	e.condition(job, cond)
	var account *corev1.ContainerState
	if bundledIdentity(e.z) {
		account = &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}
	}
	e.pod(job, account, &corev1.ContainerStateTerminated{
		ExitCode: exit, Reason: reason, Message: report,
		FinishedAt: metav1.NewTime(e.clock.Add(5 * time.Second)),
	})
}

func (e *verifyEnv) condition(job *batchv1.Job, cond batchv1.JobCondition) {
	e.t.Helper()
	job.Status.Conditions = append(job.Status.Conditions, cond)
	require.NoError(e.t, e.c.Status().Update(context.Background(), job))
}

// pod creates the run's pod; account is the init container's state, check the
// check container's terminated state (nil: it never ran).
func (e *verifyEnv) pod(job *batchv1.Job, account *corev1.ContainerState, check *corev1.ContainerStateTerminated) {
	e.t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: job.Name + "-x7k2p", Namespace: verifyNS,
		Labels: map[string]string{batchv1.JobNameLabel: job.Name},
	}}
	if account != nil {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: templates.VerifyAccountContainer, State: *account}}
	}
	state := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	if check != nil {
		state = corev1.ContainerState{Terminated: check}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: templates.VerifyCheckContainer, State: state}}
	require.NoError(e.t, e.c.Create(context.Background(), pod))
}

// report is a doctor report over the checks, counted the way zae counts them.
func report(checks ...doctorCheck) string {
	rep := doctorReport{V: 1, Zae: "v0.3.0", URL: "https://media.example.org", Checks: checks}
	for _, c := range checks {
		switch c.S {
		case "ok":
			rep.Passed++
		case "warn":
			rep.Warned++
		case "fail":
			rep.Failed++
		case "skip":
			rep.Skipped++
		}
	}
	b, _ := json.Marshal(rep)
	return string(b)
}

var healthy = []doctorCheck{
	{N: "tls", S: "ok", D: "certificate valid, 61 days left"},
	{N: "routes", S: "ok", D: "3 public paths answer"},
	{N: "oidc issuer", S: "ok", D: "https://media.example.org/auth/realms/zaentrum serves discovery"},
	{N: "sign-in", S: "ok", D: "zaentrum-verify signed in"},
	{N: "image registry", S: "warn", D: "ghcr.io unreachable from here"},
}

func cond(z *zaentrumv1alpha1.Zaentrum) *metav1.Condition {
	return meta.FindStatusCondition(z.Status.Conditions, condTypeVerified)
}

// ── starting a run ──────────────────────────────────────────────────────────

// The first pass that finds the platform Ready verifies it: a Job of its own,
// owned by the Zaentrum, from the rendered hook — never the hook itself.
func TestVerifyStartsOnceThePlatformIsReady(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)

	assert.False(t, e.pass(false), "nothing in flight while the platform is not Ready")
	assert.Nil(t, e.z.Status.Verification, "no run against a platform still rolling out")
	assert.Empty(t, e.jobs())
	assert.Nil(t, e.secret(), "nothing is prepared before there is a run")

	require.True(t, e.pass(true), "a run in flight asks for the sooner requeue")
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, v.Result)
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerUpdate, v.Trigger)
	assert.Equal(t, platformFingerprint(e.platform), v.Fingerprint)
	assert.Len(t, v.Fingerprint, 12)
	assert.Equal(t, "latest", v.Version)
	require.NotNil(t, v.StartedAt)
	assert.Equal(t, e.clock, v.StartedAt.Time)
	assert.Nil(t, v.FinishedAt)

	jobs := e.jobs()
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, v.Job, job.Name)
	assert.True(t, strings.HasPrefix(job.Name, templates.VerifyJobName+"-"), job.Name)
	assert.Len(t, job.Name, len(templates.VerifyJobName)+1+verifyJobSuffix)
	assert.True(t, metav1.IsControlledBy(&job, e.z), "the run goes with the platform")
	assert.Equal(t, verificationRun, job.Labels[labelVerification])
	for k := range job.Annotations {
		assert.False(t, strings.HasPrefix(k, "helm.sh/"), "a Helm hook annotation on an operator Job: %s", k)
	}
	assert.Equal(t, "update", job.Annotations[annotationVerifyTrigger])
	assert.Equal(t, v.Fingerprint, job.Annotations[annotationVerifyFingerprint])
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, templates.VerifyCheckContainer, job.Spec.Template.Spec.Containers[0].Name)

	c := cond(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, "Running", c.Reason)
	assert.Contains(t, c.Message, v.Fingerprint)
}

// A run in flight is polled, not restarted, and a pass that finds it still
// running changes nothing in status — a status write that moved every pass
// would re-trigger the reconcile that wrote it.
func TestVerifyRunInFlightIsLeftAlone(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	first := e.v().DeepCopy()

	require.True(t, e.pass(true))
	require.True(t, e.pass(true))
	assert.Equal(t, first, e.v(), "an unchanged run is an unchanged status")
	assert.Len(t, e.jobs(), 1, "polling never starts a second Job")

	// A pod that is merely starting holds nothing up worth saying.
	e.pod(e.job(first.Job), &corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, nil)
	require.True(t, e.pass(true))
	assert.Empty(t, e.v().Message, "PodInitializing is no reason to report")
	assert.NotContains(t, cond(e.z).Message, "waiting")
}

// ── the verdict ─────────────────────────────────────────────────────────────

func TestVerifyPassed(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	e.end(0, "Completed", report(healthy...))

	assert.False(t, e.pass(true), "nothing in flight, nothing waiting")
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, v.Result)
	assert.Equal(t, int32(4), v.Passed)
	assert.Equal(t, int32(1), v.Warned)
	assert.Equal(t, int32(0), v.Failed)
	assert.Equal(t, int32(0), v.Skipped)
	require.Len(t, v.Checks, len(healthy))
	assert.Equal(t, zaentrumv1alpha1.VerificationCheck{Name: "tls", Status: "ok", Detail: "certificate valid, 61 days left"}, v.Checks[0])
	assert.Equal(t, zaentrumv1alpha1.VerificationCheckWarn, v.Checks[4].Status)
	require.NotNil(t, v.FinishedAt)
	assert.True(t, v.StartedAt.Add(5*time.Second).Equal(v.FinishedAt.Time),
		"when the check container ended, not when the operator noticed: %s", v.FinishedAt)
	assert.Equal(t, "5 of 5 checks passed; warnings: image registry", v.Message)

	c := cond(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Passed", c.Reason)
	assert.Len(t, e.jobs(), 1, "the finished Job stays for its logs")
}

func TestVerifyFailed(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	checks := append([]doctorCheck{}, healthy...)
	checks[3] = doctorCheck{N: "sign-in", S: "fail", D: "the login form answered 502"}
	checks = append(checks, doctorCheck{N: "registered checks", S: "skip", D: "the instance offers none"})
	e.end(1, "Error", report(checks...))

	assert.False(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationFailed, v.Result)
	assert.Equal(t, int32(1), v.Failed)
	assert.Equal(t, int32(3), v.Passed)
	assert.Equal(t, int32(1), v.Skipped)
	assert.Equal(t, "1 of 6 checks failed: sign-in; warnings: image registry; skipped: registered checks", v.Message)
	c := cond(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "Failed", c.Reason)
	assert.Equal(t, v.Message, c.Message)
}

// A failed fingerprint is not run again by itself — not every 30s, not after an
// operator restart. Only a new update or a new request starts another run.
func TestVerifyNeverRerunsAFailedFingerprintByItself(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	e.end(1, "Error", report(doctorCheck{N: "routes", S: "fail", D: "/portal (404)"}))
	require.False(t, e.pass(true))
	failed := e.v().DeepCopy()

	for i := 0; i < 5; i++ {
		assert.False(t, e.pass(true))
	}
	e.render() // a fresh render of the same platform: the same fingerprint
	assert.False(t, e.pass(true))
	assert.Equal(t, failed, e.v())
	assert.Len(t, e.jobs(), 1)
}

// Every update that leaves the platform Ready is verified; the previous run's
// Job goes, so there is at most one.
func TestVerifyRunsAgainAfterAnUpdate(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	firstJob := e.v().Job
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))
	firstPrint := e.v().Fingerprint

	e.setImage("chino-api", "ghcr.io/zaentrum/chino-api@sha256:"+strings.Repeat("ab", 32))
	assert.False(t, e.pass(false), "mid-rollout: no run yet")
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, e.v().Result)

	require.True(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, v.Result)
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerUpdate, v.Trigger)
	assert.NotEqual(t, firstPrint, v.Fingerprint)
	jobs := e.jobs()
	require.Len(t, jobs, 1, "at most the latest verify Job")
	assert.NotEqual(t, firstJob, jobs[0].Name)
}

// ── requests ────────────────────────────────────────────────────────────────

// A request is any value of the annotation the operator has not answered; the
// run it starts answers it, and the same value never asks twice.
func TestVerifyRunsOnRequest(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))

	e.request("tok-1")
	assert.False(t, e.pass(false), "a request waits for a Ready platform")
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, e.v().Result)

	require.True(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, v.Result)
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerRequest, v.Trigger)
	assert.Equal(t, "tok-1", v.Request)
	assert.Contains(t, cond(e.z).Message, "on request")
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))
	assert.Equal(t, "tok-1", e.v().Request, "the verdict answers the request")

	assert.False(t, e.pass(true), "an answered request does not ask again")
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, e.v().Result)

	e.request("tok-2")
	require.True(t, e.pass(true))
	assert.Equal(t, "tok-2", e.v().Request)
}

// An update-triggered run keeps the last answered request. Clearing it — or
// answering the annotation still in place — would make that annotation read as
// a new request after every update, and verify every update twice.
func TestVerifyUpdateKeepsTheAnsweredRequest(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	e.request("tok-1")
	require.True(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerRequest, e.v().Trigger, "the first run answers a waiting request")
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))

	e.setImage("portal-api", "ghcr.io/zaentrum/portal-api@sha256:"+strings.Repeat("cd", 32))
	require.True(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerUpdate, e.v().Trigger)
	assert.Equal(t, "tok-1", e.v().Request, "carried over, not cleared")
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))

	assert.False(t, e.pass(true), "the annotation left in place is no new request")
	assert.Len(t, e.jobs(), 1)
	assert.Equal(t, "tok-1", e.v().Request)
}

// Requests queue: one written while a run is in flight neither interrupts it
// nor is dropped. The run ends and its verdict is written; the next pass starts
// the run that answers the request.
func TestVerifyRequestsQueueBehindARunInFlight(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	inFlight := e.v().Job

	e.request("tok-1")
	require.True(t, e.pass(true), "come back soon: a request is waiting")
	assert.Equal(t, inFlight, e.v().Job, "the run in flight is not interrupted")
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerUpdate, e.v().Trigger)
	assert.Empty(t, e.v().Request)
	assert.Len(t, e.jobs(), 1)

	e.end(1, "Error", report(doctorCheck{N: "routes", S: "fail", D: "/portal (404)"}))
	require.True(t, e.pass(true), "the waiting request starts on the next pass")
	assert.Equal(t, zaentrumv1alpha1.VerificationFailed, e.v().Result, "the ended run's verdict is written first")
	assert.Empty(t, e.v().Request, "an update-triggered run answers no request")

	require.True(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, v.Result)
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerRequest, v.Trigger)
	assert.Equal(t, "tok-1", v.Request)
	assert.NotEqual(t, inFlight, v.Job)
	assert.Len(t, e.jobs(), 1)
}

// An update that lands while a run is in flight waits for it as well: the run
// ends with the fingerprint it verified, and the next one verifies the new one.
func TestVerifyUpdateQueuesBehindARunInFlight(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	old := e.v().Fingerprint

	e.setImage("katalog-api", "ghcr.io/zaentrum/katalog-api@sha256:"+strings.Repeat("ef", 32))
	require.True(t, e.pass(true))
	assert.Equal(t, old, e.v().Fingerprint)
	e.end(0, "Completed", report(healthy...))
	require.True(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, e.v().Result)
	assert.Equal(t, old, e.v().Fingerprint, "the verdict names the platform it verified")

	require.True(t, e.pass(true))
	assert.Equal(t, platformFingerprint(e.platform), e.v().Fingerprint)
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, e.v().Result)
}

// ── no verdict ──────────────────────────────────────────────────────────────

// A run that ends without a readable report has no verdict: Error, and the
// message says what happened instead.
func TestVerifyErrorWithoutAReport(t *testing.T) {
	cases := map[string]struct {
		end    func(e *verifyEnv)
		want   []string
		counts bool // the report was readable, so its counts and checks stand
	}{
		"the runner exited 1 and wrote nothing": {
			end:  func(e *verifyEnv) { e.end(1, "Error", "") },
			want: []string{"exited 1 (Error) without a report"},
		},
		"OOM killed before it reported": {
			end:  func(e *verifyEnv) { e.end(137, "OOMKilled", "") },
			want: []string{"exited 137 (OOMKilled)"},
		},
		"exit 0 with a report that is not JSON": {
			end:  func(e *verifyEnv) { e.end(0, "Completed", "doctor: no failures") },
			want: []string{"exited 0 (Completed)", "could not be read", "not JSON"},
		},
		"a report format this operator does not read": {
			end:  func(e *verifyEnv) { e.end(0, "Completed", `{"v":2,"passed":1,"checks":[]}`) },
			want: []string{"format 2"},
		},
		"a check status outside the contract": {
			end: func(e *verifyEnv) {
				e.end(0, "Completed", `{"v":1,"passed":1,"checks":[{"n":"tls","s":"great","d":""}]}`)
			},
			want: []string{`"tls"`, `"great"`},
		},
		"exit 1 although the report lists no failure": {
			end:    func(e *verifyEnv) { e.end(1, "Error", report(healthy...)) },
			want:   []string{"exited 1 (Error), although its report lists no failed check"},
			counts: true,
		},
		"the deadline, with the pod gone": {
			end: func(e *verifyEnv) {
				job := e.job(e.v().Job)
				e.pod(job, nil, nil) // waiting on its image while in flight
				var pod corev1.Pod
				require.NoError(e.t, e.c.Get(context.Background(),
					types.NamespacedName{Namespace: verifyNS, Name: job.Name + "-x7k2p"}, &pod))
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: `Back-off pulling image "ghcr.io/zaentrum/zae:v9"`}}
				require.NoError(e.t, e.c.Status().Update(context.Background(), &pod))
				require.True(e.t, e.pass(true))
				assert.Contains(e.t, e.v().Message, "ImagePullBackOff", "visible while in flight")
				assert.Equal(e.t, zaentrumv1alpha1.VerificationRunning, e.v().Result)
				assert.Contains(e.t, cond(e.z).Message, "ImagePullBackOff")

				// The Job controller deletes an active pod at the deadline.
				require.NoError(e.t, e.c.Delete(context.Background(), &pod))
				e.condition(job, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
					Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"})
			},
			want: []string{"DeadlineExceeded: Job was active longer than specified deadline",
				"last seen: the doctor container is waiting: ImagePullBackOff"},
		},
		"the account could not be prepared": {
			end: func(e *verifyEnv) {
				job := e.job(e.v().Job)
				e.condition(job, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
					Reason: "BackoffLimitExceeded"})
				e.pod(job, &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error",
					Message: "cannot sign in to the Keycloak admin API at http://keycloak:80/auth as the bootstrap admin: Invalid user credentials [invalid_grant]",
				}}, nil)
			},
			want: []string{"the test account could not be prepared: cannot sign in to the Keycloak admin API",
				"invalid_grant"},
		},
		"the Job is gone before its result was read": {
			end: func(e *verifyEnv) {
				require.NoError(e.t, e.c.Delete(context.Background(), e.job(e.v().Job)))
			},
			want: []string{"is gone; its result was never read"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newVerifyEnv(t, verifyCR(), nil)
			require.True(t, e.pass(true))
			tc.end(e)

			assert.False(t, e.pass(true), "an Error is final for its fingerprint")
			v := e.v()
			assert.Equal(t, zaentrumv1alpha1.VerificationError, v.Result)
			for _, w := range tc.want {
				assert.Contains(t, v.Message, w)
			}
			if tc.counts {
				assert.NotEmpty(t, v.Checks, "a readable report keeps its checks")
			} else {
				assert.Zero(t, v.Passed+v.Failed+v.Warned+v.Skipped, "no counts without a readable report")
				assert.Empty(t, v.Checks)
			}
			require.NotNil(t, v.FinishedAt)
			c := cond(e.z)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, "Error", c.Reason)

			assert.False(t, e.pass(true), "and not retried by itself")
			assert.Equal(t, zaentrumv1alpha1.VerificationError, e.v().Result)
		})
	}
}

// A run that cannot even start is an Error for its fingerprint, not a retry
// every thirty seconds.
func TestVerifyStartFailureIsNotRetriedByItself(t *testing.T) {
	creates := 0
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Job" {
			creates++
			return apierrors.NewForbidden(batchv1.Resource("jobs"), obj.GetName(), errors.New("violates PodSecurity"))
		}
		return c.Create(ctx, obj, opts...)
	}}
	e := newVerifyEnv(t, verifyCR(), funcs)

	assert.False(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationError, v.Result)
	assert.Contains(t, v.Message, "could not start the verification Job")
	assert.Contains(t, v.Message, "violates PodSecurity")
	assert.Equal(t, platformFingerprint(e.platform), v.Fingerprint)
	require.NotNil(t, v.FinishedAt, "a run that could not start has ended")
	assert.Empty(t, v.Job)

	assert.False(t, e.pass(true))
	assert.Equal(t, 1, creates, "not retried by itself")
	e.request("tok-1")
	e.pass(true)
	assert.Equal(t, 2, creates, "a request retries it")
}

// ── the report, capped ──────────────────────────────────────────────────────

// A long report is capped — 40 checks, 200 characters of detail — and the cut
// never hides a failure.
func TestVerifyOversizedReport(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))

	var checks []doctorCheck
	for i := 0; i < 100; i++ {
		checks = append(checks, doctorCheck{N: fmt.Sprintf("check-%03d", i), S: "ok", D: strings.Repeat("ä", 220)})
	}
	checks[97].S, checks[98].S = "fail", "warn"
	checks[3].S = "fail"
	e.end(1, "Error", report(checks...))

	require.False(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationFailed, v.Result)
	assert.Equal(t, int32(2), v.Failed)
	assert.Equal(t, int32(97), v.Passed)
	require.Len(t, v.Checks, maxVerifyChecks)
	names := map[string]zaentrumv1alpha1.VerificationCheckStatus{}
	for i, c := range v.Checks {
		names[c.Name] = c.Status
		assert.LessOrEqual(t, utf8.RuneCountInString(c.Detail), maxVerifyDetail)
		assert.True(t, strings.HasSuffix(c.Detail, "…"), "the cut is marked")
		if i > 0 {
			assert.Less(t, v.Checks[i-1].Name, c.Name, "report order is kept")
		}
	}
	assert.Equal(t, zaentrumv1alpha1.VerificationCheckFail, names["check-003"])
	assert.Equal(t, zaentrumv1alpha1.VerificationCheckFail, names["check-097"], "a failure past the cut is kept")
	assert.Equal(t, zaentrumv1alpha1.VerificationCheckWarn, names["check-098"])
	assert.Contains(t, v.Message, "60 more checks not listed")
	assert.LessOrEqual(t, utf8.RuneCountInString(v.Message), maxVerifyMessage)
}

// The kubelet keeps the LAST 4 KiB of a termination message. A report larger
// than that arrives cut at the front — unreadable, so no verdict is made up.
func TestVerifyReportCutByTheKubelet(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	var checks []doctorCheck
	for i := 0; i < 60; i++ {
		checks = append(checks, doctorCheck{N: fmt.Sprintf("check-%02d", i), S: "ok", D: strings.Repeat("x", 120)})
	}
	full := report(checks...)
	require.Greater(t, len(full), 4096)
	e.end(0, "Completed", full[len(full)-4096:])

	require.False(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationError, e.v().Result)
	assert.Contains(t, e.v().Message, "its report could not be read")
}

// The report should never carry the test account's password. If it ever does,
// status does not.
func TestVerifyRedactsThePassword(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	sec := e.secret()
	require.NotNil(t, sec)
	pw := string(sec.Data["password"])
	e.end(1, "Error", report(doctorCheck{N: "sign-in", S: "fail", D: "zaentrum-verify / " + pw + " rejected"}))

	require.False(t, e.pass(true))
	blob, err := json.Marshal(e.z.Status)
	require.NoError(t, err)
	assert.NotContains(t, string(blob), pw)
	assert.Equal(t, "zaentrum-verify / [redacted] rejected", e.v().Checks[0].Detail)
}

func TestParseReport(t *testing.T) {
	rep, err := parseReport(" " + report(healthy...) + "\n")
	require.NoError(t, err)
	assert.Equal(t, int32(4), rep.Passed)
	rep, err = parseReport(`{"v":1,"zae":"v0.3.0","url":"https://media.example.org","passed":0,"failed":1,"warned":0,"skipped":0,` +
		`"checks":[{"n":"tls","s":"FAIL","d":"expired"}],"later":"fields are fine"}`)
	require.NoError(t, err, "unknown fields are ignored, status case is not significant")
	assert.Equal(t, "fail", rep.Checks[0].S)

	for name, raw := range map[string]string{
		"empty":          "",
		"not JSON":       "doctor: FAILING",
		"no version":     `{"passed":1}`,
		"newer version":  `{"v":2}`,
		"negative count": `{"v":1,"failed":-1}`,
		"unnamed check":  `{"v":1,"checks":[{"n":" ","s":"ok"}]}`,
		"unknown status": `{"v":1,"checks":[{"n":"tls","s":"meh"}]}`,
		"too large":      `{"v":1,"url":"` + strings.Repeat("x", maxVerifyReport) + `"}`,
	} {
		_, err := parseReport(raw)
		assert.Error(t, err, name)
	}
	_, err = parseReport("")
	assert.ErrorIs(t, err, errNoReport)
}

// Counts the report gives are taken as given, unless they cannot be right.
func TestVerifyCountsFromTheList(t *testing.T) {
	v := &zaentrumv1alpha1.VerificationStatus{}
	id := func(s string) string { return s }

	// The runner counted more checks than it listed: its counts stand.
	fillChecks(v, &doctorReport{V: 1, Passed: 7, Checks: []doctorCheck{{N: "a", S: "ok"}}}, id)
	assert.Equal(t, int32(7), v.Passed)

	// Counts that account for fewer checks than listed are recounted.
	fillChecks(v, &doctorReport{V: 1, Checks: []doctorCheck{{N: "a", S: "ok"}, {N: "b", S: "skip"}}}, id)
	assert.Equal(t, int32(1), v.Passed)
	assert.Equal(t, int32(1), v.Skipped)

	// "Nothing failed" beside a failed check is not believed.
	fillChecks(v, &doctorReport{V: 1, Passed: 5, Checks: []doctorCheck{{N: "a", S: "fail"}}}, id)
	assert.Equal(t, int32(1), v.Failed)
}

// ── the test account ────────────────────────────────────────────────────────

// Bundled identity: the operator makes the account's Secret once — username and
// a crypto/rand password, owned by the Zaentrum — and never rotates it.
func TestVerifyAccountSecretIsMadeOnce(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	require.True(t, e.pass(true))
	sec := e.secret()
	require.NotNil(t, sec, "made before the first run")
	assert.Equal(t, templates.VerifyAccountName, string(sec.Data["username"]))
	pw := string(sec.Data["password"])
	assert.Len(t, pw, 32)
	for _, class := range verifyPasswordClasses {
		assert.True(t, strings.ContainsAny(pw, class), "the password holds a character of %q", class)
	}
	assert.True(t, metav1.IsControlledBy(sec, e.z), "the Secret goes with the platform")
	assert.Equal(t, verificationAccount, sec.Labels[labelVerification])
	assert.Equal(t, corev1.SecretTypeOpaque, sec.Type)

	// Later runs, an update, a request: the same password.
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))
	e.request("tok-1")
	require.True(t, e.pass(true))
	assert.Equal(t, pw, string(e.secret().Data["password"]), "never rotated")
	assert.Equal(t, sec.ResourceVersion, e.secret().ResourceVersion, "not even rewritten")

	other, err := newVerifyPassword()
	require.NoError(t, err)
	assert.NotEqual(t, pw, string(other), "crypto/rand, not a constant")
}

// The Secret is the operator's own state: made even where the platform's
// secrets are external.
func TestVerifyAccountSecretWithExternalSecrets(t *testing.T) {
	z := verifyCR()
	z.Spec.Secrets.External = true
	e := newVerifyEnv(t, z, nil)
	require.True(t, e.pass(true))
	assert.NotNil(t, e.secret())
}

// Only a missing key is filled in; what is there stays.
func TestVerifyAccountSecretMissingKeyIsFilled(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: templates.VerifySecretName, Namespace: verifyNS},
		Data:       map[string][]byte{"password": []byte("kept-as-it-is-1234")},
	}
	e := newVerifyEnv(t, verifyCR(), nil, existing)
	require.True(t, e.pass(true))
	sec := e.secret()
	assert.Equal(t, "kept-as-it-is-1234", string(sec.Data["password"]))
	assert.Equal(t, templates.VerifyAccountName, string(sec.Data["username"]))

	sec.Data["password"] = nil
	require.NoError(t, e.c.Update(context.Background(), sec))
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))
	e.request("tok-1")
	require.True(t, e.pass(true))
	assert.Len(t, e.secret().Data["password"], verifyPasswordLen, "a password that went missing is made anew")
}

// External identity: the account is the admin's. Nothing is made up; the
// Secret generated for the bundled realm goes; the admin's own stays as it is.
func TestVerifyAccountWithExternalIdentity(t *testing.T) {
	external := func() *zaentrumv1alpha1.Zaentrum {
		z := verifyCR()
		z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
		z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
		return z
	}

	t.Run("nothing is made", func(t *testing.T) {
		e := newVerifyEnv(t, external(), nil)
		require.True(t, e.pass(true))
		assert.Nil(t, e.secret())
		job := e.job(e.v().Job)
		assert.Empty(t, job.Spec.Template.Spec.InitContainers, "no realm to prepare")
	})

	t.Run("the bundled realm's Secret goes", func(t *testing.T) {
		z := verifyCR()
		e := newVerifyEnv(t, z, nil)
		require.True(t, e.pass(true))
		require.NotNil(t, e.secret())
		e.end(0, "Completed", report(healthy...))
		require.False(t, e.pass(true))

		z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
		z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
		e.render()
		e.request("tok-1")
		require.True(t, e.pass(true))
		assert.Nil(t, e.secret(), "that password signs in nowhere else")
	})

	t.Run("the admin's Secret stays", func(t *testing.T) {
		mine := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: templates.VerifySecretName, Namespace: verifyNS},
			Data:       map[string][]byte{"username": []byte("checks@example.org"), "password": []byte("from-the-admin-123")},
		}
		e := newVerifyEnv(t, external(), nil, mine)
		require.True(t, e.pass(true))
		sec := e.secret()
		require.NotNil(t, sec)
		assert.Equal(t, mine.Data, sec.Data)
	})
}

// ── disabled ────────────────────────────────────────────────────────────────

func TestVerifyDisabled(t *testing.T) {
	z := verifyCR()
	e := newVerifyEnv(t, z, nil)
	e.request("tok-1")
	require.True(t, e.pass(true))
	require.Len(t, e.jobs(), 1)

	no := false
	z.Spec.Verification.Enabled = &no
	e.render()
	assert.Empty(t, e.tests, "no checks rendered")
	assert.False(t, e.pass(true))
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationSkipped, v.Result)
	assert.Empty(t, v.Fingerprint)
	assert.Equal(t, "tok-1", v.Request, "the last answered request stays")
	assert.Contains(t, v.Message, "disabled")
	assert.Empty(t, e.jobs(), "the run in flight is abandoned")
	c := cond(e.z)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, "Disabled", c.Reason)

	e.request("tok-2")
	assert.False(t, e.pass(true), "no run while disabled")
	assert.Equal(t, "tok-1", e.v().Request, "a request asked while disabled waits")
	assert.Empty(t, e.jobs())

	yes := true
	z.Spec.Verification.Enabled = &yes
	e.render()
	require.True(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, e.v().Result)
	assert.Equal(t, "tok-2", e.v().Request, "answered once verification is back")
}

// ── the fingerprint ─────────────────────────────────────────────────────────

func TestPlatformFingerprint(t *testing.T) {
	dep := func(name string, containers, inits map[string]string) *unstructured.Unstructured {
		list := func(m map[string]string) []interface{} {
			var out []interface{}
			for n, img := range m {
				out = append(out, map[string]interface{}{"name": n, "image": img})
			}
			return out
		}
		o := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]interface{}{"name": name},
			"spec": map[string]interface{}{"replicas": int64(1), "template": map[string]interface{}{
				"spec": map[string]interface{}{"containers": list(containers), "initContainers": list(inits)}}},
		}}
		return o
	}
	a := dep("chino-api", map[string]string{"app": "ghcr.io/zaentrum/chino-api@sha256:aa"}, map[string]string{"wait": "busybox:1"})
	b := dep("portal-api", map[string]string{"app": "ghcr.io/zaentrum/portal-api@sha256:bb"}, nil)
	svc := &unstructured.Unstructured{Object: map[string]interface{}{"kind": "Service", "metadata": map[string]interface{}{"name": "x"}}}
	job := dep("schema", map[string]string{"migrate": "ghcr.io/zaentrum/katalog-api@sha256:dd"}, nil)
	job.SetKind("Job")

	fp := platformFingerprint([]*unstructured.Unstructured{a, b, svc})
	assert.Regexp(t, `^[0-9a-f]{12}$`, fp)
	assert.Equal(t, fp, platformFingerprint([]*unstructured.Unstructured{b, svc, a}), "order does not matter")
	assert.Equal(t, fp, platformFingerprint([]*unstructured.Unstructured{a, b}), "only Deployments count")
	assert.Equal(t, fp, platformFingerprint([]*unstructured.Unstructured{a, job, b}), "a Job's pod template is no Deployment")

	// The contract: sha256 over the sorted lines, first 12 hex characters.
	lines := "chino-api/app=ghcr.io/zaentrum/chino-api@sha256:aa\nchino-api/wait=busybox:1\nportal-api/app=ghcr.io/zaentrum/portal-api@sha256:bb"
	assert.Equal(t, sha256Hex(lines)[:12], fp)

	scaled := dep("chino-api", map[string]string{"app": "ghcr.io/zaentrum/chino-api@sha256:aa"}, map[string]string{"wait": "busybox:1"})
	scaled.Object["spec"].(map[string]interface{})["replicas"] = int64(3)
	assert.Equal(t, fp, platformFingerprint([]*unstructured.Unstructured{scaled, b}), "scaling is not an update")

	moved := dep("chino-api", map[string]string{"app": "ghcr.io/zaentrum/chino-api@sha256:cc"}, map[string]string{"wait": "busybox:1"})
	assert.NotEqual(t, fp, platformFingerprint([]*unstructured.Unstructured{moved, b}), "an image is")
	initMoved := dep("chino-api", map[string]string{"app": "ghcr.io/zaentrum/chino-api@sha256:aa"}, map[string]string{"wait": "busybox:2"})
	assert.NotEqual(t, fp, platformFingerprint([]*unstructured.Unstructured{initMoved, b}), "an init container's too")
}

// ── it touches nothing else ─────────────────────────────────────────────────

// Verification is a reading beside the platform's status: a run, a failure or
// an Error never moves the phase, the Ready condition or a component.
func TestVerifyTouchesNothingElse(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	e.z.Status = zaentrumv1alpha1.ZaentrumStatus{
		Phase: "Ready", CurrentVersion: "latest", ObservedGeneration: 1,
		Components: []zaentrumv1alpha1.ComponentStatus{{Name: "chino-api", Ready: true}},
		Conditions: []metav1.Condition{{Type: condTypeReady, Status: metav1.ConditionTrue, Reason: "AllComponentsReady",
			Message: "all components are ready", LastTransitionTime: metav1.Now()}},
	}
	before := e.z.Status.DeepCopy()
	only := func() zaentrumv1alpha1.ZaentrumStatus {
		s := *e.z.Status.DeepCopy()
		s.Verification = nil
		var conds []metav1.Condition
		for _, c := range s.Conditions {
			if c.Type != condTypeVerified {
				conds = append(conds, c)
			}
		}
		s.Conditions = conds
		return s
	}

	require.True(t, e.pass(true))
	assert.Equal(t, *before, only())
	e.end(1, "Error", report(doctorCheck{N: "routes", S: "fail"}))
	require.False(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationFailed, e.v().Result)
	assert.Equal(t, *before, only(), "a failed verification is not a degraded platform")
}

// ── the reconcile ───────────────────────────────────────────────────────────

// The whole loop through Reconcile: the test hooks are never applied, a Ready
// platform starts a run and is polled every 10s, its verdict lands in status,
// and the platform's phase is the platform's.
func TestReconcileVerifiesAReadyPlatform(t *testing.T) {
	z := verifyCR()
	s := selfScheme(t)
	var applied []string
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			applied = append(applied, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
			return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
		}}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}
	get := func() *zaentrumv1alpha1.Zaentrum {
		var got zaentrumv1alpha1.Zaentrum
		require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
		return &got
	}

	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, requeueAfter, res.RequeueAfter)
	assert.NotEmpty(t, applied)
	for _, a := range applied {
		assert.NotEqual(t, "Job/"+templates.VerifyJobName, a, "the test hook was applied with the platform")
		assert.NotEqual(t, "Secret/"+templates.VerifySecretName, a, "the hook's Secret was applied with the platform")
	}
	assert.Nil(t, get().Status.Verification, "Progressing: no run yet")
	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS)))
	assert.Empty(t, jobs.Items)

	// Every Deployment becomes available.
	var deps appsv1.DeploymentList
	require.NoError(t, c.List(ctx, &deps, client.InNamespace(verifyNS)))
	require.NotEmpty(t, deps.Items)
	for i := range deps.Items {
		d := &deps.Items[i]
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		d.Status.AvailableReplicas = desired
		d.Status.UpdatedReplicas = desired
		require.NoError(t, c.Status().Update(ctx, d))
	}

	res, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "polled while a run is in flight")
	got := get()
	assert.Equal(t, "Ready", got.Status.Phase)
	require.NotNil(t, got.Status.Verification)
	assert.Equal(t, zaentrumv1alpha1.VerificationRunning, got.Status.Verification.Result)
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS)))
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0]

	// The run fails a check. The platform stays Ready.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	require.NoError(t, c.Status().Update(ctx, &job))
	require.NoError(t, c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-abcde", Namespace: verifyNS,
			Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: templates.VerifyCheckContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", Message: report(doctorCheck{N: "routes", S: "fail", D: "/portal (404)"}),
			}},
		}}},
	}))

	res, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, requeueAfter, res.RequeueAfter)
	got = get()
	assert.Equal(t, "Ready", got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, condTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "a failed check is not a degraded platform")
	require.NotNil(t, got.Status.Verification)
	assert.Equal(t, zaentrumv1alpha1.VerificationFailed, got.Status.Verification.Result)
	verified := meta.FindStatusCondition(got.Status.Conditions, condTypeVerified)
	require.NotNil(t, verified)
	assert.Equal(t, metav1.ConditionFalse, verified.Status)

	res, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, requeueAfter, res.RequeueAfter)
	require.NoError(t, c.List(ctx, &jobs, client.InNamespace(verifyNS)))
	assert.Len(t, jobs.Items, 1, "a failed fingerprint is not run again")
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// An annotation removed after its request was answered is no request, and an
// update-triggered run still keeps the value it answered.
func TestVerifyRemovedAnnotationKeepsTheAnsweredRequest(t *testing.T) {
	e := newVerifyEnv(t, verifyCR(), nil)
	e.request("tok-1")
	require.True(t, e.pass(true))
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))

	e.z.SetAnnotations(nil)
	e.setImage("chino-web", "ghcr.io/zaentrum/chino-web@sha256:"+strings.Repeat("12", 32))
	require.True(t, e.pass(true))
	assert.Equal(t, zaentrumv1alpha1.VerificationTriggerUpdate, e.v().Trigger)
	assert.Equal(t, "tok-1", e.v().Request)
}

// The previous run's Job goes with background propagation: the Job now, its pod
// right after — never orphaned, never waited on.
func TestVerifyDeletesThePreviousRunInTheBackground(t *testing.T) {
	var policies []metav1.DeletionPropagation
	funcs := &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			if o.PropagationPolicy != nil {
				policies = append(policies, *o.PropagationPolicy)
			} else {
				policies = append(policies, "")
			}
		}
		return c.Delete(ctx, obj, opts...)
	}}
	e := newVerifyEnv(t, verifyCR(), funcs)
	require.True(t, e.pass(true))
	e.end(0, "Completed", report(healthy...))
	require.False(t, e.pass(true))
	e.request("tok-1")
	require.True(t, e.pass(true))
	assert.Equal(t, []metav1.DeletionPropagation{metav1.DeletePropagationBackground}, policies)
}

// Every generated password holds every class a realm policy may ask for — not
// by luck.
func TestVerifyPasswordClasses(t *testing.T) {
	seen := map[string]bool{}
	upperFirst := 0
	for i := 0; i < 300; i++ {
		pw, err := newVerifyPassword()
		require.NoError(t, err)
		require.Len(t, pw, 32)
		if strings.ContainsRune(verifyPasswordClasses[0], rune(pw[0])) {
			upperFirst++
		}
		for _, class := range verifyPasswordClasses {
			require.True(t, strings.ContainsAny(string(pw), class), "%q lacks a character of %q", pw, class)
		}
		require.False(t, seen[string(pw)], "a password came out twice")
		seen[string(pw)] = true
	}
	assert.Less(t, upperFirst, 250, "the guaranteed characters are shuffled, not left in class order")
}

func TestJobFinished(t *testing.T) {
	for name, tc := range map[string]struct {
		conds        []batchv1.JobCondition
		done, failed bool
	}{
		"running":      {nil, false, false},
		"complete":     {[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}, true, false},
		"failed":       {[]batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}, true, true},
		"not yet":      {[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}, {Type: batchv1.JobFailed, Status: corev1.ConditionFalse}}, false, false},
		"interim only": {[]batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}}, false, false},
		"suspended":    {[]batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}, false, false},
	} {
		done, failed := jobFinished(&batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conds}})
		assert.Equal(t, tc.done, done, name)
		assert.Equal(t, tc.failed, failed != nil, name)
	}
}

// The result is read from the pod whose check container ended — the last one,
// if the Job had to replace a pod.
func TestVerifyReadsThePodThatRan(t *testing.T) {
	ended := func(at time.Time, msg string) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: templates.VerifyCheckContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Message: msg, FinishedAt: metav1.NewTime(at)}},
		}}}}
	}
	evicted := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

	assert.Nil(t, chooseRunPod(nil))
	got := chooseRunPod([]corev1.Pod{evicted, ended(t0, "first")})
	require.NotNil(t, got)
	assert.Equal(t, "first", checkTerminated(got).Message, "not the pod that never ran")
	got = chooseRunPod([]corev1.Pod{ended(t0.Add(time.Minute), "later"), ended(t0, "earlier"), evicted})
	assert.Equal(t, "later", checkTerminated(got).Message)
	got = chooseRunPod([]corev1.Pod{ended(t0, "earlier"), ended(t0.Add(time.Minute), "later")})
	assert.Equal(t, "later", checkTerminated(got).Message)
	assert.Equal(t, &evicted, chooseRunPod([]corev1.Pod{evicted}))
}

func TestClip(t *testing.T) {
	exact := strings.Repeat("é", maxVerifyDetail)
	assert.Equal(t, exact, clip(exact, maxVerifyDetail), "exactly the limit stays whole")
	over := clip(exact+"x", maxVerifyDetail)
	assert.Equal(t, maxVerifyDetail, utf8.RuneCountInString(over))
	assert.True(t, strings.HasSuffix(over, "…"))
	assert.Equal(t, "a\uFFFDb", clip("a\xffb", 10), "invalid UTF-8 is made valid")
	assert.Equal(t, "trimmed", clip("  trimmed \n", 10))
}
