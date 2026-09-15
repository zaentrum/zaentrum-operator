package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
)

func testNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addonNS}}
}

// removedAddon is the example addon in the middle of being deleted.
func removedAddon(keep bool) *zaentrumv1alpha1.ZaentrumAddon {
	a := testAddon(false)
	a.Finalizers = []string{addon.FinalizerValues}
	now := metav1.Now()
	a.DeletionTimestamp = &now
	if keep {
		a.Annotations = map[string]string{addon.AnnotationKeepValues: "true"}
	}
	return a
}

// valuesSecret is a values Secret the portal created for addon, owned by
// ownerUID ("" = unowned), created age ago.
func valuesSecret(name, addonName string, ownerUID types.UID, age time.Duration, now time.Time) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: addonNS,
		Labels:            map[string]string{addon.LabelAddon: addonName},
		CreationTimestamp: metav1.NewTime(now.Add(-age)),
	}}
	if ownerUID != "" {
		s.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: zaentrumv1alpha1.GroupVersion.String(), Kind: "ZaentrumAddon", Name: addonName, UID: ownerUID,
		}}
	}
	return s
}

func getSecret(t *testing.T, c client.Client, name string) (*corev1.Secret, bool) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &s, true
}

func addonGone(t *testing.T, c client.Client) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: "example"}, &zaentrumv1alpha1.ZaentrumAddon{})
	return apierrors.IsNotFound(err)
}

func reconcileRemoval(t *testing.T, r *ZaentrumAddonReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: addonNS, Name: "example"}})
	return err
}

// Every addon carries the values finalizer after its first reconcile.
func TestAddonAddsValuesFinalizer(t *testing.T) {
	r, _ := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true), testValuesSecret())
	_, a := reconcileExample(t, r)
	assert.Contains(t, a.Finalizers, addon.FinalizerValues)
	assert.Equal(t, zaentrumv1alpha1.AddonPlanned, a.Status.Phase, "status is still written after the finalizer patch")

	// A second pass does not add it twice.
	_, a = reconcileExample(t, r)
	count := 0
	for _, f := range a.Finalizers {
		if f == addon.FinalizerValues {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

// Removed without keep-values: the finalizer just comes off; owner references
// leave the values to garbage collection.
func TestAddonRemovedWithoutKeep(t *testing.T) {
	now := time.Now()
	values := valuesSecret("zaentrum-addon-example-values-x7k2p", "example", "addon-uid", time.Hour, now)
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testNamespace(), removedAddon(false), values)

	require.NoError(t, reconcileRemoval(t, r))
	assert.True(t, addonGone(t, c), "finalizer removed, addon deleted")
	s, ok := getSecret(t, c, values.Name)
	require.True(t, ok)
	assert.Len(t, s.OwnerReferences, 1, "still owned: garbage collection removes it")
	assert.Empty(t, s.Labels[addon.LabelKeep])
}

// Removed with keep-values: this addon's owner references come off its values
// and generated Secrets, they are labelled keep, and nothing else is touched.
func TestAddonRemovedKeepsValues(t *testing.T) {
	now := time.Now()
	yes := true
	values := valuesSecret("zaentrum-addon-example-values-x7k2p", "example", "addon-uid", time.Hour, now)
	values.OwnerReferences = append(values.OwnerReferences, metav1.OwnerReference{
		APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: "other-uid",
	})
	generated := valuesSecret("zaentrum-addon-example-generated", "example", "", time.Hour, now)
	generated.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: zaentrumv1alpha1.GroupVersion.String(), Kind: "ZaentrumAddon", Name: "example", UID: "addon-uid", Controller: &yes,
	}}
	// A chart-applied Secret labelled for the addon but not a values Secret, and
	// another addon's values Secret: both left alone.
	applied := valuesSecret("worker", "example", "addon-uid", time.Hour, now)
	other := valuesSecret("zaentrum-addon-other-values-abc", "other", "other-addon-uid", time.Hour, now)

	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testNamespace(), removedAddon(true),
		values, generated, applied, other)
	require.NoError(t, reconcileRemoval(t, r))
	assert.True(t, addonGone(t, c))

	for _, name := range []string{values.Name, generated.Name} {
		s, ok := getSecret(t, c, name)
		require.True(t, ok, name)
		assert.Equal(t, "true", s.Labels[addon.LabelKeep], name)
		for _, ref := range s.OwnerReferences {
			assert.NotEqual(t, types.UID("addon-uid"), ref.UID, "%s keeps no reference to the removed addon", name)
		}
	}
	s, _ := getSecret(t, c, values.Name)
	require.Len(t, s.OwnerReferences, 1, "an unrelated owner reference stays")
	assert.Equal(t, types.UID("other-uid"), s.OwnerReferences[0].UID)

	s, _ = getSecret(t, c, applied.Name)
	assert.Empty(t, s.Labels[addon.LabelKeep], "a chart-applied Secret is not a values Secret")
	assert.Len(t, s.OwnerReferences, 1)
	s, _ = getSecret(t, c, other.Name)
	assert.Empty(t, s.Labels[addon.LabelKeep], "another addon's values are untouched")
}

// The finalizer never holds a removal for good: with the namespace terminating
// or gone, or no platform left, it comes off without keeping anything.
func TestAddonRemovedLetsGo(t *testing.T) {
	now := time.Now()
	terminating := testNamespace()
	terminating.Finalizers = []string{"kubernetes"}
	del := metav1.Now()
	terminating.DeletionTimestamp = &del

	for name, objs := range map[string][]client.Object{
		"namespace terminating": {testPlatform(), terminating},
		"namespace gone":        {testPlatform()},
		"no platform":           {testNamespace()},
	} {
		t.Run(name, func(t *testing.T) {
			values := valuesSecret("zaentrum-addon-example-values-x7k2p", "example", "addon-uid", time.Hour, now)
			objs := append(append([]client.Object{}, objs...), removedAddon(true), values)
			r, c := newAddonReconciler(t, exampleCharts(t), objs...)
			require.NoError(t, reconcileRemoval(t, r))
			assert.True(t, addonGone(t, c), "finalizer removed")
			s, ok := getSecret(t, c, values.Name)
			require.True(t, ok)
			assert.Empty(t, s.Labels[addon.LabelKeep], "nothing kept")
		})
	}
}

// newInterceptedReconciler builds a reconciler whose fake client fails Secret
// patches with patchErr.
func newInterceptedReconciler(t *testing.T, patchErr error, objs ...client.Object) (*ZaentrumAddonReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&zaentrumv1alpha1.ZaentrumAddon{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if gvk := obj.GetObjectKind().GroupVersionKind(); gvk.Kind == "Secret" {
					return patchErr
				}
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
		}).
		Build()
	return &ZaentrumAddonReconciler{Client: c, Scheme: scheme, APIReader: c, Charts: exampleCharts(t)}, c
}

// A values Secret that vanishes while it is being kept does not hold the
// removal: the finalizer comes off anyway.
func TestAddonRemovedKeepSecretVanished(t *testing.T) {
	values := valuesSecret("zaentrum-addon-example-values-x7k2p", "example", "addon-uid", time.Hour, time.Now())
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, values.Name)
	r, c := newInterceptedReconciler(t, notFound, testPlatform(), testNamespace(), removedAddon(true), values)
	require.NoError(t, reconcileRemoval(t, r))
	assert.True(t, addonGone(t, c))
}

// Any other failure while keeping is retried: the addon stays until its values
// are really handed back.
func TestAddonRemovedKeepErrorRetries(t *testing.T) {
	values := valuesSecret("zaentrum-addon-example-values-x7k2p", "example", "addon-uid", time.Hour, time.Now())
	r, c := newInterceptedReconciler(t, errors.New("api unavailable"), testPlatform(), testNamespace(), removedAddon(true), values)
	assert.ErrorContains(t, reconcileRemoval(t, r), "keep values secret zaentrum-addon-example-values-x7k2p: api unavailable")
	assert.False(t, addonGone(t, c), "the finalizer holds until the values are kept")
}

// On reconcile, exactly the addon's unreferenced values Secrets past the grace
// are collected; everything else stays.
func TestCollectValuesSecrets(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	yes := true
	a := testAddon(true) // valuesFrom -> zaentrum-addon-example-values (testValuesSecret)
	referenced := testValuesSecret()
	referenced.CreationTimestamp = metav1.NewTime(now.Add(-30 * time.Minute))

	gone := []*corev1.Secret{
		valuesSecret("zaentrum-addon-example-values-old", "example", "addon-uid", 20*time.Minute, now),
		valuesSecret("zaentrum-addon-example-values-unowned", "example", "", 11*time.Minute, now),
	}
	young := valuesSecret("zaentrum-addon-example-values-young", "example", "", 2*time.Minute, now)
	kept := valuesSecret("zaentrum-addon-example-values-kept", "example", "", time.Hour, now)
	kept.Labels[addon.LabelKeep] = "true"
	foreign := valuesSecret("zaentrum-addon-example-values-foreign", "example", "someone-else", time.Hour, now)
	mixed := valuesSecret("zaentrum-addon-example-values-mixed", "example", "addon-uid", time.Hour, now)
	mixed.OwnerReferences = append(mixed.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "cm-uid"})
	generated := valuesSecret("zaentrum-addon-example-generated", "example", "", time.Hour, now)
	generated.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: zaentrumv1alpha1.GroupVersion.String(), Kind: "ZaentrumAddon", Name: "example", UID: "addon-uid", Controller: &yes,
	}}
	chartSecret := valuesSecret("worker", "example", "addon-uid", time.Hour, now)
	mislabelled := valuesSecret("zaentrum-addon-example-values-mislabelled", "other", "", time.Hour, now)
	otherAddon := valuesSecret("zaentrum-addon-example-values-x", "example-values", "", time.Hour, now) // addon "example-values" extends the prefix

	objs := []client.Object{testPlatform(), a, referenced, young, kept, foreign, mixed, generated, chartSecret, mislabelled, otherAddon}
	for _, s := range gone {
		objs = append(objs, s)
	}
	r, c := newAddonReconciler(t, exampleCharts(t), objs...)
	r.Now = func() time.Time { return now }

	again, err := r.collectValuesSecrets(context.Background(), a)
	require.NoError(t, err)
	assert.Equal(t, 8*time.Minute+time.Second, again, "look again when the young Secret leaves its grace")

	for _, s := range gone {
		_, ok := getSecret(t, c, s.Name)
		assert.False(t, ok, "%s is collected", s.Name)
	}
	for _, s := range []*corev1.Secret{referenced, young, kept, foreign, mixed, generated, chartSecret, mislabelled, otherAddon} {
		_, ok := getSecret(t, c, s.Name)
		assert.True(t, ok, "%s stays", s.Name)
	}
}

// Through a full reconcile, collection runs and a Secret still inside its grace
// pulls the next reconcile forward.
func TestReconcileRequeuesForValuesGrace(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	young := valuesSecret("zaentrum-addon-example-values-young", "example", "", valuesSecretGrace-5*time.Second, now)
	old := valuesSecret("zaentrum-addon-example-values-old", "example", "", time.Hour, now)
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true), testValuesSecret(), young, old)
	r.Now = func() time.Time { return now }

	res, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonPlanned, a.Status.Phase)
	assert.Equal(t, 6*time.Second, res.RequeueAfter, "sooner than the settled requeue")
	_, ok := getSecret(t, c, old.Name)
	assert.False(t, ok)
	_, ok = getSecret(t, c, young.Name)
	assert.True(t, ok)
}

// The sweep deletes values Secrets of addons that do not exist, past an hour
// and not kept — per namespace — and nothing else.
func TestSweepOrphanedValues(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	inNS := func(s *corev1.Secret, ns string) *corev1.Secret { s.Namespace = ns; return s }

	orphans := []*corev1.Secret{
		valuesSecret("zaentrum-addon-gone-values-a", "gone", "", 2*time.Hour, now),
		valuesSecret("zaentrum-addon-gone-values-b", "gone", "stale-addon-uid", 2*time.Hour, now), // owner was the deleted addon
		inNS(valuesSecret("zaentrum-addon-example-values-y", "example", "", 2*time.Hour, now), "elsewhere"),
	}
	young := valuesSecret("zaentrum-addon-gone-values-young", "gone", "", 30*time.Minute, now)
	kept := valuesSecret("zaentrum-addon-gone-values-kept", "gone", "", 2*time.Hour, now)
	kept.Labels[addon.LabelKeep] = "true"
	live := valuesSecret("zaentrum-addon-example-values-x", "example", "", 2*time.Hour, now) // its addon exists here
	generated := valuesSecret("zaentrum-addon-gone-generated", "gone", "", 2*time.Hour, now)
	foreign := valuesSecret("zaentrum-addon-gone-values-foreign", "gone", "", 2*time.Hour, now)
	foreign.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "cm-uid"}}
	mismatch := valuesSecret("zaentrum-addon-other-values-z", "gone", "", 2*time.Hour, now)
	unlabelled := valuesSecret("zaentrum-addon-gone-values-nolabel", "gone", "", 2*time.Hour, now)
	unlabelled.Labels = nil

	objs := []client.Object{testPlatform(), testAddon(true), young, kept, live, generated, foreign, mismatch, unlabelled}
	for _, s := range orphans {
		objs = append(objs, s)
	}
	r, c := newAddonReconciler(t, exampleCharts(t), objs...)
	r.Now = func() time.Time { return now }
	require.NoError(t, r.SweepOrphanedValues(context.Background()))

	get := func(s *corev1.Secret) bool {
		err := c.Get(context.Background(), types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &corev1.Secret{})
		if apierrors.IsNotFound(err) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	for _, s := range orphans {
		assert.False(t, get(s), "%s/%s is swept", s.Namespace, s.Name)
	}
	for _, s := range []*corev1.Secret{young, kept, live, generated, foreign, mismatch, unlabelled} {
		assert.True(t, get(s), "%s stays", s.Name)
	}
}

func TestSooner(t *testing.T) {
	assert.Equal(t, time.Duration(0), sooner(0, 0))
	assert.Equal(t, 3*time.Second, sooner(0, 3*time.Second))
	assert.Equal(t, 3*time.Second, sooner(3*time.Second, 0))
	assert.Equal(t, 2*time.Second, sooner(3*time.Second, 2*time.Second))
	assert.Equal(t, 2*time.Second, sooner(2*time.Second, 3*time.Second))
}
