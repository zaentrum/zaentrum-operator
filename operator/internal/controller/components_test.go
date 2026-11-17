package controller

import (
	"context"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// Since when a part of the platform is not ready, status.components[].notReadySince
// (spec/portal/operations.md, Alerts, Where each fact comes from): the time a
// part was first seen not ready while it is meant to run, absent while it is
// ready and while it is meant to run at 0.

// componentsOf is the status's components by name.
func componentsOf(z *zaentrumv1alpha1.Zaentrum) map[string]zaentrumv1alpha1.ComponentStatus {
	out := map[string]zaentrumv1alpha1.ComponentStatus{}
	for _, c := range z.Status.Components {
		out[c.Name] = c
	}
	return out
}

// setAvailable gives a Deployment every replica it asks for, available, as the
// Deployment controller reports a part that is up; or none.
func (e *platformEnv) setAvailable(name string, up bool) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.key.Namespace, Name: name}, &dep))
	n := int32(0)
	if up {
		n = 1
		if dep.Spec.Replicas != nil {
			n = *dep.Spec.Replicas
		}
	}
	dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas = n, n
	require.NoError(e.t, e.c.Status().Update(ctx, &dep))
}

// sinceIs says that a part's notReadySince is the time want.
func sinceIs(t *testing.T, want time.Time, c zaentrumv1alpha1.ComponentStatus, msg string) {
	t.Helper()
	if assert.NotNil(t, c.NotReadySince, "%s: %s has no notReadySince", msg, c.Name) {
		assert.True(t, c.NotReadySince.Time.Equal(want), "%s: %s has been not ready since %s, not %s", msg, c.Name, c.NotReadySince.Time.UTC(), want.UTC())
	}
}

// R.C6: a part that is not ready though it is meant to run says since when: the
// time of the pass that first saw it so, kept on every pass after it, also by
// an operator that starts again, which reads it back from the status. Once the
// part is ready it has none, and when it falls again it counts from then.
func TestRC6APartSaysSinceWhenItIsNotReady(t *testing.T) {
	e := newPlatformEnv(t, verifyCR(), nil)
	first := e.clock
	e.reconcile()
	comps := componentsOf(e.zaentrum())
	require.Contains(t, comps, "chino-api")
	require.Contains(t, comps, "keycloak")
	for _, c := range comps {
		assert.False(t, c.Ready, c.Name)
		sinceIs(t, first, c, "no part is up on a fake API server")
	}

	e.clock = e.clock.Add(5 * time.Minute)
	e.reconcile()
	for _, c := range componentsOf(e.zaentrum()) {
		sinceIs(t, first, c, "five minutes later")
	}

	e.setAvailable("chino-api", true)
	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	comps = componentsOf(e.zaentrum())
	assert.True(t, comps["chino-api"].Ready)
	assert.Nil(t, comps["chino-api"].NotReadySince, "a part that is ready has none")
	sinceIs(t, first, comps["keycloak"], "the others keep theirs")

	e.setAvailable("chino-api", false)
	e.clock = e.clock.Add(time.Minute)
	fell := e.clock
	e.reconcile()
	comps = componentsOf(e.zaentrum())
	assert.False(t, comps["chino-api"].Ready)
	sinceIs(t, fell, comps["chino-api"], "it fell again")

	// The operator starts again: a reconciler of its own, over the same API
	// server, later.
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: e.r.Scheme, Now: func() time.Time { return e.clock }}
	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	comps = componentsOf(e.zaentrum())
	sinceIs(t, fell, comps["chino-api"], "an operator that starts again")
	sinceIs(t, first, comps["keycloak"], "an operator that starts again")
}

// endRestoreJob ends the restore's Job as the Job controller and the kubelet
// do, with exit 0.
func (e *platformEnv) endRestoreJob() {
	e.t.Helper()
	ctx := context.Background()
	z := e.zaentrum()
	require.NotNil(e.t, z.Status.Backup)
	require.NotNil(e.t, z.Status.Backup.Restore)
	var job batchv1.Job
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: z.Status.Backup.Restore.Job}, &job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(e.t, e.c.Status().Update(ctx, &job))
	require.NoError(e.t, e.c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: z.Namespace,
		Labels: map[string]string{batchv1.JobNameLabel: job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: templates.RestoreContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "restored chino, katalog, keycloak, portal from " + aDump}}}}}}))
}

// R.C6: a part meant to run at 0 has no notReadySince — one an admin stopped,
// which spec.replicas names with 0, and every client of the database the
// operator keeps stopped for a restore — and one meant to run again counts
// from the pass that sees it so, not from before it was stopped.
func TestRC6APartMeantToRunAtZeroHasNoNotReadySince(t *testing.T) {
	t.Run("R.C6 one an admin stopped", func(t *testing.T) {
		z := verifyCR()
		z.Spec.Replicas = map[string]int32{"chino-api": 0}
		e := newPlatformEnv(t, z, nil)
		e.reconcile()
		comps := componentsOf(e.zaentrum())
		assert.False(t, comps["chino-api"].Ready)
		assert.Nil(t, comps["chino-api"].NotReadySince, "spec.replicas names 0 for it")
		sinceIs(t, e.clock, comps["chino-web"], "a part meant to run")

		e.clock = e.clock.Add(30 * time.Minute)
		started := e.clock
		e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Replicas = map[string]int32{"chino-api": 1} })
		e.reconcile()
		sinceIs(t, started, componentsOf(e.zaentrum())["chino-api"], "meant to run again")
	})

	t.Run("R.C6 every part the operator keeps stopped for a restore", func(t *testing.T) {
		e := newPlatformEnv(t, verifyCR(), nil)
		first := e.clock
		e.reconcile()

		e.change(func(z *zaentrumv1alpha1.Zaentrum) {
			z.Annotations = map[string]string{zaentrumv1alpha1.RestoreRequestAnnotation: aDump}
		})
		e.clock = e.clock.Add(time.Minute)
		e.reconcile()
		got := e.zaentrum()
		require.Equal(t, "Restoring", got.Status.Phase)
		var dep appsv1.Deployment
		require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "keycloak"}, &dep))
		require.Equal(t, int32(0), *dep.Spec.Replicas, "a client of the database, stopped")
		comps := componentsOf(got)
		assert.Nil(t, comps["keycloak"].NotReadySince, "kept stopped for the restore")
		assert.Nil(t, comps["chino-api"].NotReadySince, "kept stopped for the restore")
		sinceIs(t, first, comps["chino-web"], "no client of the database: it runs on")

		e.endRestoreJob()
		e.clock = e.clock.Add(10 * time.Minute)
		e.reconcile()
		got = e.zaentrum()
		require.Equal(t, zaentrumv1alpha1.RestoreSucceeded, got.Status.Backup.Restore.Result)
		assert.Nil(t, componentsOf(got)["keycloak"].NotReadySince, "the pass that reads the end still renders it stopped")

		e.clock = e.clock.Add(time.Minute)
		started := e.clock
		e.reconcile()
		require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "keycloak"}, &dep))
		require.Equal(t, int32(1), *dep.Spec.Replicas, "started again")
		comps = componentsOf(e.zaentrum())
		sinceIs(t, started, comps["keycloak"], "counted from its start, not from the restore's")
		sinceIs(t, started, comps["chino-api"], "counted from its start, not from the restore's")
		sinceIs(t, first, comps["chino-web"], "it ran on")
	})

	t.Run("R.C6 a part whose Deployment is not there yet", func(t *testing.T) {
		s := selfScheme(t)
		r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s,
			Now: func() time.Time { return tlsNow }}
		deployment := func(name string, replicas int64) *unstructured.Unstructured {
			u := &unstructured.Unstructured{}
			u.SetAPIVersion("apps/v1")
			u.SetKind("Deployment")
			u.SetNamespace(verifyNS)
			u.SetName(name)
			require.NoError(t, unstructured.SetNestedField(u.Object, replicas, "spec", "replicas"))
			return u
		}
		z := verifyCR()
		ready, err := r.refreshComponents(context.Background(), z, []*unstructured.Unstructured{
			deployment("chino-api", 1), deployment("keycloak", 0)})
		require.NoError(t, err)
		assert.False(t, ready)
		comps := componentsOf(z)
		sinceIs(t, tlsNow, comps["chino-api"], "meant to run, and not there yet")
		assert.Nil(t, comps["keycloak"].NotReadySince, "applied at 0")
	})
}

// setRollout makes a Deployment's rollout one that has given up, as the
// Deployment controller reports it once progressDeadlineSeconds have passed
// with no new pod while what ran before still serves; or one that went on, its
// new pods updated and available.
func (e *platformEnv) setRollout(name string, gaveUp bool) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	require.NoError(e.t, e.c.Get(ctx, types.NamespacedName{Namespace: e.key.Namespace, Name: name}, &dep))
	dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas = 1, 1
	progressing := appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}
	if gaveUp {
		dep.Status.UpdatedReplicas = 0
		progressing = appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}
	}
	dep.Status.Conditions = []appsv1.DeploymentCondition{progressing}
	require.NoError(e.t, e.c.Status().Update(ctx, &dep))
}

// storedComponents are the status's components as they are stored, by name:
// what the operator's types write, so a field left out is absent there.
func (e *platformEnv) storedComponents() map[string]map[string]any {
	e.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(zaentrumv1alpha1.GroupVersion.WithKind("Zaentrum"))
	require.NoError(e.t, e.c.Get(context.Background(), e.key, u))
	list, _, err := unstructured.NestedSlice(u.Object, "status", "components")
	require.NoError(e.t, err)
	out := map[string]map[string]any{}
	for _, c := range list {
		m, _ := c.(map[string]any)
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

// R.C6: a part whose own rollout has given up says so, stalled: true, beside
// the Ready condition's message that names it — and is ready all the same
// while what ran before still serves, so it has no notReadySince. A part that
// is fine has no stalled, absent from the status as stored, and the field
// clears once the rollout goes on.
func TestRC6APartWhoseRolloutHasGivenUpIsStalled(t *testing.T) {
	e := newPlatformEnv(t, verifyCR(), nil)
	e.reconcile()
	e.setAvailable("chino-web", true)
	e.setRollout("chino-api", true)
	e.clock = e.clock.Add(time.Minute)
	e.reconcile()

	z := e.zaentrum()
	comps := componentsOf(z)
	assert.True(t, comps["chino-api"].Stalled, "its rollout has given up")
	assert.True(t, comps["chino-api"].Ready, "what ran before still serves")
	assert.Nil(t, comps["chino-api"].NotReadySince, "a stalled part that still serves is ready")
	assert.False(t, comps["chino-web"].Stalled, "a part that is fine")
	ready := meta.FindStatusCondition(z.Status.Conditions, condTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, "RolloutStalled", ready.Reason)
	assert.Contains(t, ready.Message, "chino-api (ProgressDeadlineExceeded, 0/1 updated replicas)")
	stored := e.storedComponents()
	assert.Equal(t, true, stored["chino-api"]["stalled"])
	assert.NotContains(t, stored["chino-web"], "stalled", "absent, not false")

	e.setRollout("chino-api", false)
	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	assert.False(t, componentsOf(e.zaentrum())["chino-api"].Stalled, "the rollout went on")
	assert.NotContains(t, e.storedComponents()["chino-api"], "stalled", "absent once it went on")
}
