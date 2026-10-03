package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// realmEnv is one platform and the realm passes taken over it, with a fake API
// server under them and a clock the test moves.
type realmEnv struct {
	t        *testing.T
	c        client.WithWatch
	r        *ZaentrumReconciler
	z        *zaentrumv1alpha1.Zaentrum
	platform []*unstructured.Unstructured
	hooks    []*unstructured.Unstructured
	clock    time.Time
}

func newRealmEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum) *realmEnv {
	t.Helper()
	s := selfScheme(t)
	e := &realmEnv{t: t, z: z, clock: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)}
	e.c = fake.NewClientBuilder().WithScheme(s).WithObjects(z).
		WithStatusSubresource(&appsv1.Deployment{}, &batchv1.Job{}).Build()
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	e.render()
	return e
}

func (e *realmEnv) render() {
	e.t.Helper()
	objs, err := templates.Render(templates.NewValues(e.z))
	require.NoError(e.t, err)
	e.platform, e.hooks = templates.SplitHooks(objs)
}

// keycloak puts the platform's Keycloak Deployment in place, rolled out, with
// available of its one replica available.
func (e *realmEnv) keycloak(available int32) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	err := e.c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: keycloakDeployment}, &dep)
	if err != nil {
		one := int32(1)
		dep = appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: keycloakDeployment, Namespace: verifyNS},
			Spec: appsv1.DeploymentSpec{Replicas: &one}}
		require.NoError(e.t, e.c.Create(ctx, &dep))
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, UpdatedReplicas: 1, AvailableReplicas: available}
	require.NoError(e.t, e.c.Status().Update(ctx, &dep))
}

func (e *realmEnv) pass() bool {
	e.t.Helper()
	e.clock = e.clock.Add(10 * time.Second)
	return e.r.configureRealm(context.Background(), e.z, e.platform, e.hooks)
}

func (e *realmEnv) runs() []batchv1.Job {
	e.t.Helper()
	jobs, err := e.r.realmRuns(context.Background(), e.z)
	require.NoError(e.t, err)
	return jobs
}

func (e *realmEnv) cond() *metav1.Condition {
	return meta.FindStatusCondition(e.z.Status.Conditions, condTypeRealmConfigured)
}

// end finishes the latest run the way the Job controller and the kubelet do.
func (e *realmEnv) end(ok bool, message string) {
	e.t.Helper()
	ctx := context.Background()
	runs := e.runs()
	require.NotEmpty(e.t, runs)
	job := runs[0]
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.clock)}
	exit := int32(0)
	if !ok {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			LastTransitionTime: metav1.NewTime(e.clock)}
		exit = 1
	}
	job.Status.Conditions = append(job.Status.Conditions, cond)
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x1", Namespace: verifyNS, Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.RealmContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message}}}}},
	}))
}

// A realm run starts once Keycloak is available: a Job of its own, owned by
// the Zaentrum, from the rendered hook, without its Helm annotations.
func TestRealmRunStartsOnceKeycloakIsAvailable(t *testing.T) {
	e := newRealmEnv(t, verifyCR())

	assert.False(t, e.pass())
	assert.Empty(t, e.runs(), "no Keycloak, nothing to configure")
	assert.Equal(t, "WaitingForKeycloak", e.cond().Reason)
	assert.Equal(t, metav1.ConditionUnknown, e.cond().Status)

	e.keycloak(0)
	assert.False(t, e.pass())
	assert.Empty(t, e.runs(), "Keycloak still starting")
	assert.Contains(t, e.cond().Message, "0 of 1 replicas available")

	// A new spec the Deployment controller has not rolled out yet: the old
	// pods are still the ones that answer.
	e.keycloak(1)
	var dep appsv1.Deployment
	require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: keycloakDeployment}, &dep))
	dep.Status.ObservedGeneration = dep.Generation - 1
	require.NoError(t, e.c.Status().Update(context.Background(), &dep))
	assert.False(t, e.pass())
	assert.Empty(t, e.runs(), "Keycloak is rolling out")
	assert.Contains(t, e.cond().Message, "a new Keycloak is rolling out")

	e.keycloak(1)
	require.True(t, e.pass(), "a run in flight asks for the sooner requeue")
	runs := e.runs()
	require.Len(t, runs, 1)
	job := runs[0]
	assert.True(t, strings.HasPrefix(job.Name, templates.RealmJobName+"-"), job.Name)
	assert.True(t, metav1.IsControlledBy(&job, e.z))
	assert.Equal(t, realmRun, job.Labels[labelRealm])
	for k := range job.Annotations {
		assert.False(t, strings.HasPrefix(k, "helm.sh/"), "a Helm hook annotation on an operator Job: %s", k)
	}
	hook := templates.RealmJob(e.hooks)
	assert.Equal(t, realmFingerprint(hook), job.Annotations[annotationRealmFingerprint])
	var fromHook batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(hook.Object, &fromHook))
	assert.Equal(t, fromHook.Spec.Template.Spec.Containers, job.Spec.Template.Spec.Containers, "the hook's own pod")
	assert.Equal(t, "Configuring", e.cond().Reason)

	// The run in flight is left alone.
	require.True(t, e.pass())
	assert.Len(t, e.runs(), 1)
}

// A finished run reports what it did, and the same platform starts no other.
func TestRealmRunSucceeded(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	require.True(t, e.pass())
	e.end(true, "set the redirects of chino-web zaentrum-web; already so: chino-tv zae")

	assert.False(t, e.pass())
	c := e.cond()
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Configured", c.Reason)
	assert.Equal(t, "set the redirects of chino-web zaentrum-web; already so: chino-tv zae", c.Message)
	for i := 0; i < 3; i++ {
		assert.False(t, e.pass())
	}
	assert.Len(t, e.runs(), 1, "nothing changed, nothing runs")
}

// What the realm should hold changes with the platform: the next pass runs
// again, the previous run's Job goes.
func TestRealmRunsAgainWhenTheRedirectsChange(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	require.True(t, e.pass())
	first := e.runs()[0]
	e.end(true, "set the redirects of chino-web")
	assert.False(t, e.pass())

	e.z.Spec.Hostname = "media2.example.org"
	e.render()
	require.True(t, e.pass())
	runs := e.runs()
	require.Len(t, runs, 1, "at most the latest run is kept")
	assert.NotEqual(t, first.Name, runs[0].Name)
	assert.NotEqual(t, first.Annotations[annotationRealmFingerprint], runs[0].Annotations[annotationRealmFingerprint])
}

// A change that comes while a run is in flight waits for it to end.
func TestRealmChangeWaitsForTheRunInFlight(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	require.True(t, e.pass())
	first := e.runs()[0].Name

	e.z.Spec.Hostname = "media2.example.org"
	e.render()
	require.True(t, e.pass())
	runs := e.runs()
	require.Len(t, runs, 1)
	assert.Equal(t, first, runs[0].Name, "one run at a time")

	e.end(true, "set the redirects of chino-web")
	require.True(t, e.pass(), "the change runs once the run in flight has ended")
	runs = e.runs()
	require.Len(t, runs, 1)
	assert.NotEqual(t, first, runs[0].Name)
	assert.Equal(t, realmFingerprint(templates.RealmJob(e.hooks)), runs[0].Annotations[annotationRealmFingerprint])
}

// A failed run says why, and runs again only after a while.
func TestRealmFailedRunRunsAgainAfterAWhile(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	require.True(t, e.pass())
	first := e.runs()[0].Name
	e.end(false, "cannot sign in to the Keycloak admin API at http://keycloak:80/auth as the bootstrap admin: Invalid user credentials [invalid_grant]")

	assert.False(t, e.pass())
	c := e.cond()
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "Failed", c.Reason)
	assert.Contains(t, c.Message, first+" failed: cannot sign in to the Keycloak admin API")
	assert.Contains(t, c.Message, "it runs again after 2026-10-04T09:")
	e.clock = e.clock.Add(realmRetryAfter - time.Minute)
	assert.False(t, e.pass())
	assert.Equal(t, first, e.runs()[0].Name, "not before realmRetryAfter")

	e.clock = e.clock.Add(2 * time.Minute)
	require.True(t, e.pass())
	runs := e.runs()
	require.Len(t, runs, 1)
	assert.NotEqual(t, first, runs[0].Name, "tried again")
}

// A run's Job that is gone — a day after it succeeded — is run again, so a
// redirect added by hand does not stay.
func TestRealmRunsAgainOnceTheLastRunIsGone(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	require.True(t, e.pass())
	e.end(true, "already so: chino-web")
	assert.False(t, e.pass())
	job := e.runs()[0]
	require.NoError(t, e.c.Delete(context.Background(), &job))

	require.True(t, e.pass())
	assert.Len(t, e.runs(), 1)
}

// External identity has no realm of the platform's: no run, no condition.
func TestRealmWithExternalIdentity(t *testing.T) {
	z := verifyCR()
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	z.Status.Conditions = []metav1.Condition{{Type: condTypeRealmConfigured, Status: metav1.ConditionTrue, Reason: "Configured"}}
	e := newRealmEnv(t, z)
	e.keycloak(1)
	assert.False(t, e.pass())
	assert.Empty(t, e.runs())
	assert.Nil(t, e.cond())
}

// A realm run moves no phase, no Ready condition and nothing else in status.
func TestRealmTouchesNothingElse(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.z.Status = zaentrumv1alpha1.ZaentrumStatus{Phase: "Ready", Conditions: []metav1.Condition{
		{Type: condTypeReady, Status: metav1.ConditionTrue, Reason: "AllComponentsReady", LastTransitionTime: metav1.Now()}}}
	before := e.z.Status.DeepCopy()
	only := func() zaentrumv1alpha1.ZaentrumStatus {
		s := *e.z.Status.DeepCopy()
		meta.RemoveStatusCondition(&s.Conditions, condTypeRealmConfigured)
		return s
	}
	e.keycloak(1)
	require.True(t, e.pass())
	e.end(false, "cannot look up the client chino-web")
	assert.False(t, e.pass())
	assert.Equal(t, *before, only(), "a failed realm run is not a degraded platform")
}

// The fingerprint follows what the run would set — the client list, the
// script, the image — and nothing about the Job's name.
func TestRealmFingerprint(t *testing.T) {
	render := func(mutate func(*zaentrumv1alpha1.Zaentrum)) string {
		z := verifyCR()
		mutate(z)
		objs, err := templates.Render(templates.NewValues(z))
		require.NoError(t, err)
		_, hooks := templates.SplitHooks(objs)
		return realmFingerprint(templates.RealmJob(hooks))
	}
	same := render(func(*zaentrumv1alpha1.Zaentrum) {})
	assert.Len(t, same, 12)
	assert.Equal(t, same, render(func(*zaentrumv1alpha1.Zaentrum) {}), "stable")
	assert.NotEqual(t, same, render(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Hostname = "other.example.org" }))
	assert.NotEqual(t, same, render(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Keycloak.Image = "quay.io/keycloak/keycloak:26.0.8" }))
	assert.Equal(t, same, render(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Replicas = map[string]int32{"chino-api": 3} }),
		"what the realm holds does not follow a replica count")
}

// A realm run never starts beside a verification run, whose account
// preparation holds a master token a realm run may end.
func TestRealmWaitsForAVerificationRun(t *testing.T) {
	e := newRealmEnv(t, verifyCR())
	e.keycloak(1)
	e.z.Status.Verification = &zaentrumv1alpha1.VerificationStatus{Result: zaentrumv1alpha1.VerificationRunning, Job: "zaentrum-verify-abcde"}
	require.True(t, e.pass(), "come back soon: the check ends within minutes")
	assert.Empty(t, e.runs())
	assert.Equal(t, "Waiting", e.cond().Reason)
	assert.Contains(t, e.cond().Message, "zaentrum-verify-abcde")

	e.z.Status.Verification.Result = zaentrumv1alpha1.VerificationPassed
	require.True(t, e.pass())
	assert.Len(t, e.runs(), 1, "once the check has ended")
}
