package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

var sccKind = schema.GroupVersionKind{Group: "security.openshift.io", Version: "v1", Kind: "SecurityContextConstraints"}

// openShiftMapper is a REST mapper for a cluster that serves the SCC API.
func openShiftMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(sccKind, meta.RESTScopeRoot)
	return m
}

// failingMapper is a REST mapper whose discovery fails, as during an API
// server restart: it gives no answer at all, neither yes nor no.
type failingMapper struct {
	meta.RESTMapper
	calls int
}

func (f *failingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	f.calls++
	return nil, errors.New("unable to retrieve the complete list of server APIs: the server is currently unable to handle the request")
}

func TestOpenShiftIsTheSCCAPI(t *testing.T) {
	plain := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(selfScheme(t)).Build()}
	got, err := plain.openShift()
	require.NoError(t, err)
	assert.False(t, got, "a cluster without security.openshift.io is not OpenShift")
	require.NotNil(t, plain.OpenShift, "the answer is kept")

	os := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(selfScheme(t)).WithRESTMapper(openShiftMapper()).Build()}
	got, err = os.openShift()
	require.NoError(t, err)
	assert.True(t, got)
}

func TestOpenShiftWithoutAnAnswerIsAskedAgain(t *testing.T) {
	m := &failingMapper{RESTMapper: meta.NewDefaultRESTMapper(nil)}
	r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(selfScheme(t)).WithRESTMapper(m).Build()}
	_, err := r.openShift()
	assert.Error(t, err)
	assert.Nil(t, r.OpenShift, "a failure is not an answer to keep")
	_, err = r.openShift()
	assert.Error(t, err)
	assert.Equal(t, 2, m.calls, "asked again on the next pass")
}

func TestOpenShiftSetIsNotAsked(t *testing.T) {
	m := &failingMapper{RESTMapper: meta.NewDefaultRESTMapper(nil)}
	yes := true
	r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(selfScheme(t)).WithRESTMapper(m).Build(), OpenShift: &yes}
	got, err := r.openShift()
	require.NoError(t, err)
	assert.True(t, got)
	assert.Zero(t, m.calls)
}

// Without an answer the reconcile renders nothing and applies nothing: either
// guess would name the wrong user for every pod.
func TestReconcileDefersTheRenderWithoutAnAnswer(t *testing.T) {
	z := verifyCR()
	s := selfScheme(t)
	applied := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z).WithRESTMapper(&failingMapper{RESTMapper: meta.NewDefaultRESTMapper(nil)}).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			applied++
			return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
		}}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}})
	require.NoError(t, err)
	assert.Equal(t, verifyRequeueAfter, res.RequeueAfter, "asked again soon")
	assert.Zero(t, applied, "nothing applied without an answer")
}

// The answer reaches the render: on OpenShift the pods name no user.
func TestReconcileRendersForTheClusterItRunsOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mapper meta.RESTMapper
		user   bool
	}{
		{"kubernetes", meta.NewDefaultRESTMapper(nil), true},
		{"openshift", openShiftMapper(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := verifyCR()
			s := selfScheme(t)
			b := fake.NewClientBuilder().WithScheme(s).WithObjects(z).WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
				WithInterceptorFuncs(interceptor.Funcs{Patch: applyAsCreateOrUpdate})
			if tc.name == "openshift" {
				b = b.WithRESTMapper(tc.mapper)
			}
			c := b.Build()
			r := &ZaentrumReconciler{Client: c, Scheme: s}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}})
			require.NoError(t, err)
			var api appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "chino-api"}, &api))
			sc := api.Spec.Template.Spec.SecurityContext
			require.NotNil(t, sc)
			if tc.user {
				require.NotNil(t, sc.RunAsUser, "off OpenShift the pod names its user")
				assert.Equal(t, int64(65532), *sc.RunAsUser)
			} else {
				assert.Nil(t, sc.RunAsUser, "on OpenShift the SCC names it")
			}
		})
	}
}
