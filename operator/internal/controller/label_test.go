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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// platformEnv is a Zaentrum on a fake API server that takes a server-side
// apply as a create or an update, the reconciler over it, and a clock.
type platformEnv struct {
	t     *testing.T
	c     client.WithWatch
	r     *ZaentrumReconciler
	key   types.NamespacedName
	clock time.Time
}

// newPlatformEnv builds the env; funcs, when given, intercepts the client's
// calls besides the apply.
func newPlatformEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, funcs *interceptor.Funcs, objs ...client.Object) *platformEnv {
	t.Helper()
	s := selfScheme(t)
	// The kinds the prune lists that client-go's scheme lacks — Routes,
	// Certificates — as a cluster that serves them would answer: the fake
	// client lists only what its scheme knows.
	for _, gvk := range templates.PruneKinds {
		if !s.Recognizes(gvk) {
			s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		}
	}
	f := interceptor.Funcs{Patch: applyAsCreateOrUpdate}
	if funcs != nil {
		f.Delete, f.List = funcs.Delete, funcs.List
	}
	e := &platformEnv{t: t, key: types.NamespacedName{Namespace: z.Namespace, Name: z.Name},
		clock: time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)}
	e.c = fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}, &batchv1.Job{}).
		WithInterceptorFuncs(f).Build()
	e.r = &ZaentrumReconciler{Client: e.c, Scheme: s, Now: func() time.Time { return e.clock }}
	return e
}

// reconcile takes one pass.
func (e *platformEnv) reconcile() {
	e.t.Helper()
	_, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: e.key})
	require.NoError(e.t, err)
}

// zaentrum reads the Zaentrum back.
func (e *platformEnv) zaentrum() *zaentrumv1alpha1.Zaentrum {
	e.t.Helper()
	var z zaentrumv1alpha1.Zaentrum
	require.NoError(e.t, e.c.Get(context.Background(), e.key, &z))
	return &z
}

// change edits the Zaentrum's spec, as a person would.
func (e *platformEnv) change(edit func(*zaentrumv1alpha1.Zaentrum)) {
	e.t.Helper()
	z := e.zaentrum()
	edit(z)
	require.NoError(e.t, e.c.Update(context.Background(), z))
}

// Everything the operator applies for the platform names the Zaentrum it is
// applied for, zaentrum.io/platform, in its metadata only: no pod template and
// no selector carries it, so nothing rolls for it.
func TestEveryAppliedObjectNamesItsZaentrum(t *testing.T) {
	z := verifyCR()
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = "cpu"
	e := newPlatformEnv(t, z, nil)
	e.reconcile()

	objs, err := templates.Render(templates.NewValues(z))
	require.NoError(t, err)
	platform, _ := templates.SplitHooks(objs)
	require.NotEmpty(t, platform)
	for _, o := range platform {
		live := &metav1.PartialObjectMetadata{}
		live.SetGroupVersionKind(o.GroupVersionKind())
		require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: z.Namespace, Name: o.GetName()}, live),
			"%s/%s", o.GetKind(), o.GetName())
		assert.Equal(t, "zaentrum", live.Labels[templates.LabelPlatform], "%s/%s", o.GetKind(), o.GetName())
	}

	var deps appsv1.DeploymentList
	require.NoError(t, e.c.List(context.Background(), &deps, client.InNamespace(z.Namespace)))
	require.NotEmpty(t, deps.Items)
	for _, d := range deps.Items {
		assert.NotContains(t, d.Spec.Template.Labels, templates.LabelPlatform, "%s: the pods would roll", d.Name)
		assert.NotContains(t, d.Spec.Selector.MatchLabels, templates.LabelPlatform, "%s: a selector does not change", d.Name)
	}
}

// The label's value is the Zaentrum's name; a name longer than a label value
// may be keeps 54 of its characters and a hash of the whole, so it still
// labels, and two such names stay apart.
func TestThePlatformLabelIsAlwaysALabelValue(t *testing.T) {
	assert.Equal(t, "zaentrum", platformLabel(verifyCR()))
	long := func(tail string) *zaentrumv1alpha1.Zaentrum {
		return &zaentrumv1alpha1.Zaentrum{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("media.example.org-", 4) + tail}}
	}
	a, b := platformLabel(long("one")), platformLabel(long("two"))
	for _, v := range []string{a, b} {
		assert.Empty(t, validation.IsValidLabelValue(v), v)
		assert.Len(t, v, 63)
	}
	assert.NotEqual(t, a, b)
}
