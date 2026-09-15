package controller

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
)

// leakChart is the example chart plus a cooperating template that surfaces
// whatever it is handed in .Values.leak.dbPassword.
func leakChart(t *testing.T) *staticCharts {
	t.Helper()
	chrt, err := loader.LoadDir("../addon/testdata/example")
	require.NoError(t, err)
	chrt.Templates = append(chrt.Templates, &chart.File{Name: "templates/leak.yaml", Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata: {name: exfil}
data:
  stolen: {{ .Values.leak.dbPassword | default "" | quote }}
`)})
	path, err := chartutil.Save(chrt, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return &staticCharts{archive: &addon.Archive{Data: data, Digest: addon.Digest(data)}}
}

// Finding D, regression: valuesFrom naming a platform Secret the addon does not
// own is refused WITHOUT reading it — no exfil ConfigMap, and the operator
// never adopts the platform Secret.
func TestAddonValuesFromForeignSecretRefused(t *testing.T) {
	platformSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "chino-db-credentials", Namespace: addonNS},
		Data:       map[string][]byte{"password": []byte("PLATFORM-DB-PASSWORD")},
	}
	a := testAddon(false)
	a.Spec.ValuesFrom = []zaentrumv1alpha1.AddonValuesReference{
		{Kind: "Secret", Name: "zaentrum-addon-example-values", ValuesKey: "auth.token", TargetPath: "auth.token"},
		{Kind: "Secret", Name: "chino-db-credentials", ValuesKey: "password", TargetPath: "leak.dbPassword"},
	}
	r, c := newAddonReconciler(t, leakChart(t), testPlatform(), a, testValuesSecret(), platformSecret)
	ctx := context.Background()

	_, got := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, got.Status.Phase)
	require.NotNil(t, got.Status.Plan)
	assert.Contains(t, got.Status.Plan.ValuesErrors,
		"valuesFrom Secret/chino-db-credentials: "+valuesReadDenied)

	err := c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "exfil"}, &corev1.ConfigMap{})
	assert.True(t, apierrors.IsNotFound(err), "nothing was applied")

	var still corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "chino-db-credentials"}, &still))
	assert.Empty(t, still.OwnerReferences, "the platform Secret is never adopted")
}

// A Secret with the addon's name prefix but not the addon's label is still not
// the addon's, and is refused.
func TestAddonValuesFromWrongLabelRefused(t *testing.T) {
	mislabelled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "zaentrum-addon-example-values", Namespace: addonNS,
			Labels: map[string]string{addon.LabelAddon: "someone-else"},
		},
		Data: map[string][]byte{"auth.token": []byte("t0ken")},
	}
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true), mislabelled)
	ctx := context.Background()

	_, got := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonPlanFailed, got.Status.Phase)
	require.NotNil(t, got.Status.Plan)
	assert.Contains(t, got.Status.Plan.ValuesErrors,
		"valuesFrom Secret/zaentrum-addon-example-values: "+valuesReadDenied)

	var still corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "zaentrum-addon-example-values"}, &still))
	assert.Empty(t, still.OwnerReferences, "a mislabelled object is not adopted")
}

// A crafted chart cannot leak a secret input into status.plan.valuesErrors: a
// template error carries only its location.
func TestAddonRenderErrorHidesSecret(t *testing.T) {
	chrt, err := loader.LoadDir("../addon/testdata/example")
	require.NoError(t, err)
	chrt.Templates = append(chrt.Templates, &chart.File{Name: "templates/leak.yaml",
		Data: []byte(`{{ fail (printf "TOKEN=%s" .Values.auth.token) }}`)})
	path, err := chartutil.Save(chrt, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	r, _ := newAddonReconciler(t, &staticCharts{archive: &addon.Archive{Data: data, Digest: addon.Digest(data)}},
		testPlatform(), testAddon(true), testValuesSecret())
	_, got := reconcileExample(t, r)
	require.NotNil(t, got.Status.Plan)
	joined := ""
	for _, e := range got.Status.Plan.ValuesErrors {
		joined += e + "\n"
	}
	assert.Contains(t, joined, "template error at example/templates/leak.yaml")
	assert.NotContains(t, joined, "t0ken", "the secret input never reaches status")
	assert.NotContains(t, got.Status.Message, "t0ken")
}

// An installed addon pod gets automountServiceAccountToken:false by default.
func TestAddonInstallDefaultsAutomountFalse(t *testing.T) {
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(false), testValuesSecret())
	_, _ = reconcileExample(t, r)
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: "worker"}, &dep))
	require.NotNil(t, dep.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *dep.Spec.Template.Spec.AutomountServiceAccountToken)
}
