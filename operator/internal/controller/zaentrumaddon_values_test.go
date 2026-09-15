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
