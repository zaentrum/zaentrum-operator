package controller

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
)

const (
	testVPNProxy  = "socks5h://vpn.vpn.svc.cluster.local:1080"
	vpnPolicyName = "zaentrum-addon-example-vpn"
)

// vpnCharts is the example chart plus an uplink Deployment whose pods the
// values assign to the VPN: its pod template carries zaentrum.io/vpn with the
// value of .Values.vpnLabel, when set, and its program is handed the proxy.
func vpnCharts(t *testing.T) *staticCharts {
	t.Helper()
	chrt, err := loader.LoadDir("../addon/testdata/example")
	require.NoError(t, err)
	chrt.Templates = append(chrt.Templates, &chart.File{Name: "templates/uplink.yaml", Data: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: uplink
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: uplink
  template:
    metadata:
      labels:
        app.kubernetes.io/name: uplink
        {{- with .Values.vpnLabel }}
        zaentrum.io/vpn: {{ . | quote }}
        {{- end }}
    spec:
      containers:
        - name: uplink
          image: registry.example.org/example/uplink:1.0.0
          env:
            - name: ALL_PROXY
              value: {{ .Values.zaentrum.vpn.proxy | quote }}
`)})
	path, err := chartutil.Save(chrt, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return &staticCharts{archive: &addon.Archive{Data: data, Digest: addon.Digest(data)}}
}

// vpnPlatform is the test platform with a VPN.
func vpnPlatform() *zaentrumv1alpha1.Zaentrum {
	z := testPlatform()
	z.Spec.VPN = zaentrumv1alpha1.VPNSpec{Proxy: testVPNProxy, ClusterCIDRs: []string{"10.42.0.0/16", "10.43.0.0/16"}}
	return z
}

// vpnAddon is the example addon, installed, its uplink's pods assigned to the
// VPN by the label value given ("" for no label).
func vpnAddon(label string) *zaentrumv1alpha1.ZaentrumAddon {
	a := testAddon(false)
	values := `{"greeting":"hi"}`
	if label != "" {
		values = `{"greeting":"hi","vpnLabel":"` + label + `"}`
	}
	a.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(values)}
	return a
}

func getPolicy(t *testing.T, c client.Client) (*networkingv1.NetworkPolicy, bool) {
	t.Helper()
	var p networkingv1.NetworkPolicy
	err := c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: vpnPolicyName}, &p)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &p, true
}

func getDeployment(t *testing.T, c client.Client, name string) (*appsv1.Deployment, bool) {
	t.Helper()
	var d appsv1.Deployment
	err := c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: name}, &d)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &d, true
}

func envOf(d *appsv1.Deployment) map[string]string {
	env := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// An addon whose chart assigns pods to the VPN gets their kill switch: one
// NetworkPolicy, owned by the addon and labelled as its other objects are,
// that selects the addon's pods assigned to the VPN — not its other pods — and
// lets them connect to the cluster's networks alone. It is in the plan, and
// it is applied before the workloads.
func TestAddonVPNKillSwitch(t *testing.T) {
	var applied []string
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(vpnPlatform(), vpnAddon("true"), testValuesSecret()).
		WithStatusSubresource(&zaentrumv1alpha1.ZaentrumAddon{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() == types.ApplyPatchType {
					applied = append(applied, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
				}
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
		}).
		Build()
	r := &ZaentrumAddonReconciler{Client: c, Scheme: scheme, APIReader: c, Charts: vpnCharts(t)}

	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonInstalling, a.Status.Phase, a.Status.Message)
	assert.Empty(t, a.Status.Plan.Violations)
	assert.Contains(t, objectKeys(a.Status.Plan.Objects), "NetworkPolicy/"+vpnPolicyName, "the plan names the kill switch")
	assert.Contains(t, a.Status.Plan.Changes.Added, "NetworkPolicy/"+vpnPolicyName)

	p, ok := getPolicy(t, c)
	require.True(t, ok, "the kill switch is applied")
	owner := metav1.GetControllerOf(p)
	require.NotNil(t, owner, "owned by the addon, so its removal removes it")
	assert.Equal(t, "ZaentrumAddon", owner.Kind)
	assert.EqualValues(t, "addon-uid", owner.UID)
	assert.Equal(t, map[string]string{
		"zaentrum.io/addon":            "example",
		"app.kubernetes.io/managed-by": "zaentrum-operator",
		"app.kubernetes.io/instance":   "example",
		"app.kubernetes.io/part-of":    "zaentrum-beta-addons",
	}, p.Labels)
	assert.Equal(t, metav1.LabelSelector{MatchLabels: map[string]string{"zaentrum.io/addon": "example", "zaentrum.io/vpn": "true"}}, p.Spec.PodSelector)
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, p.Spec.PolicyTypes)
	assert.Equal(t, []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "10.42.0.0/16"}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "10.43.0.0/16"}},
	}}}, p.Spec.Egress)

	selector, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
	require.NoError(t, err)
	uplink, ok := getDeployment(t, c, "uplink")
	require.True(t, ok)
	worker, ok := getDeployment(t, c, "worker")
	require.True(t, ok)
	assert.True(t, selector.Matches(labels.Set(uplink.Spec.Template.Labels)), "it selects the uplink's pods, assigned to the VPN")
	assert.False(t, selector.Matches(labels.Set(worker.Spec.Template.Labels)), "and not the worker's")
	assert.Equal(t, testVPNProxy, envOf(uplink)["ALL_PROXY"], "the program is handed the proxy")

	policyAt, firstWorkload := -1, -1
	for i, key := range applied {
		switch key {
		case "NetworkPolicy/" + vpnPolicyName:
			policyAt = i
		case "Deployment/worker", "Deployment/uplink":
			if firstWorkload < 0 {
				firstWorkload = i
			}
		}
	}
	require.GreaterOrEqual(t, policyAt, 0, "applied: %v", applied)
	assert.Less(t, policyAt, firstWorkload, "the kill switch before any workload: %v", applied)
}

// The chart no longer assigns pods to the VPN: the kill switch is no longer
// rendered, and is pruned like the addon's other objects — once the last pod
// assigned to the VPN has stopped, so that none rolls out of the VPN without it.
func TestAddonVPNKillSwitchPruned(t *testing.T) {
	r, c := newAddonReconciler(t, vpnCharts(t), vpnPlatform(), vpnAddon("true"), testValuesSecret())
	ctx := context.Background()
	_, _ = reconcileExample(t, r)
	_, ok := getPolicy(t, c)
	require.True(t, ok)

	// A pod of the old rollout still runs, assigned to the VPN; a finished one
	// does not count, nor does another addon's.
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "uplink-old", Namespace: addonNS, Labels: map[string]string{
			"app.kubernetes.io/name": "uplink", addon.LabelAddon: "example", addon.LabelVPN: "true",
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	done := running.DeepCopy()
	done.Name, done.Status.Phase = "uplink-done", corev1.PodSucceeded
	others := running.DeepCopy()
	others.Name, others.Labels[addon.LabelAddon] = "other-uplink", "other"
	for _, p := range []*corev1.Pod{running, done, others} {
		require.NoError(t, c.Create(ctx, p))
	}

	var a zaentrumv1alpha1.ZaentrumAddon
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "example"}, &a))
	a.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"greeting":"hi"}`)}
	a.Generation++
	require.NoError(t, c.Update(ctx, &a))
	ready := func() {
		for _, name := range []string{"worker", "uplink"} {
			d, ok := getDeployment(t, c, name)
			require.True(t, ok, name)
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
			require.NoError(t, c.Status().Update(ctx, d))
		}
	}
	ready()

	res, got := reconcileExample(t, r)
	assert.Equal(t, []string{"NetworkPolicy/" + vpnPolicyName}, got.Status.Plan.Changes.Removed)
	assert.NotContains(t, objectKeys(got.Status.Plan.Objects), "NetworkPolicy/"+vpnPolicyName)
	uplink, ok := getDeployment(t, c, "uplink")
	require.True(t, ok)
	assert.NotContains(t, uplink.Spec.Template.Labels, addon.LabelVPN, "the uplink rolls out of the VPN")
	_, ok = getPolicy(t, c)
	assert.True(t, ok, "kept while a pod assigned to the VPN runs")
	assert.Equal(t, addonRequeueRollout, res.RequeueAfter, "and looked at again soon, though the addon is ready")

	require.NoError(t, c.Delete(ctx, running))
	ready()
	res, got = reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonReady, got.Status.Phase, got.Status.Message)
	_, ok = getPolicy(t, c)
	assert.False(t, ok, "pruned once none runs")
	assert.Equal(t, addonRequeueSettled, res.RequeueAfter)
	_, ok = getDeployment(t, c, "uplink")
	assert.True(t, ok, "the uplink stays, outside the VPN")
}

// An addon that assigns no pod to the VPN gets no NetworkPolicy, on a platform
// with a VPN too — and is told the proxy all the same.
func TestAddonWithoutVPNPodsGetsNoKillSwitch(t *testing.T) {
	r, c := newAddonReconciler(t, vpnCharts(t), vpnPlatform(), vpnAddon(""), testValuesSecret())
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonInstalling, a.Status.Phase, a.Status.Message)
	_, ok := getPolicy(t, c)
	assert.False(t, ok)
	assert.NotContains(t, objectKeys(a.Status.Plan.Objects), "NetworkPolicy/"+vpnPolicyName)
	uplink, ok := getDeployment(t, c, "uplink")
	require.True(t, ok)
	assert.Equal(t, testVPNProxy, envOf(uplink)["ALL_PROXY"], "zaentrum.vpn.proxy, set")
}

// zaentrum.vpn.proxy is the platform's: a proxy a user sets there is replaced,
// as the rest of the reserved block is — and so is one on a platform without a
// VPN, where it is "".
func TestAddonVPNProxyOverridesUserValues(t *testing.T) {
	for name, z := range map[string]*zaentrumv1alpha1.Zaentrum{"with a VPN": vpnPlatform(), "without": testPlatform()} {
		t.Run(name, func(t *testing.T) {
			a := vpnAddon("")
			a.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"greeting":"hi","zaentrum":{"vpn":{"proxy":"socks5://proxy.example.org:1080"}}}`)}
			r, c := newAddonReconciler(t, vpnCharts(t), z, a, testValuesSecret())
			_, _ = reconcileExample(t, r)
			uplink, ok := getDeployment(t, c, "uplink")
			require.True(t, ok)
			assert.Equal(t, z.Spec.VPN.Proxy, envOf(uplink)["ALL_PROXY"])
		})
	}
}

// zaentrum.vpn.proxy is "" on a platform without a VPN.
func TestAddonVPNProxyEmptyWithoutVPN(t *testing.T) {
	r, c := newAddonReconciler(t, vpnCharts(t), testPlatform(), vpnAddon(""), testValuesSecret())
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonInstalling, a.Status.Phase, a.Status.Message)
	uplink, ok := getDeployment(t, c, "uplink")
	require.True(t, ok)
	value, set := envOf(uplink)["ALL_PROXY"]
	assert.True(t, set)
	assert.Equal(t, "", value, "zaentrum.vpn.proxy, empty")
	_, ok = getPolicy(t, c)
	assert.False(t, ok)
}

// A pod assigned to the VPN on a platform without one would have no internet:
// the plan is refused, and nothing at all is applied.
func TestAddonVPNPodsWithoutPlatformVPN(t *testing.T) {
	r, c := newAddonReconciler(t, vpnCharts(t), testPlatform(), vpnAddon("true"), testValuesSecret())
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	want := `Deployment/uplink: label zaentrum.io/vpn: "true" assigns the pods to the platform VPN, but the platform has none (spec.vpn.proxy): they would have no internet`
	assert.Equal(t, []string{want}, a.Status.Plan.Violations)
	assert.Equal(t, want, a.Status.Message)
	assert.False(t, meta.IsStatusConditionTrue(a.Status.Conditions, condTypePlanned))
	for _, name := range []string{"uplink", "worker"} {
		_, ok := getDeployment(t, c, name)
		assert.False(t, ok, "%s is not applied", name)
	}
	_, ok := getPolicy(t, c)
	assert.False(t, ok)
}

// zaentrum.io/vpn takes "true" alone.
func TestAddonVPNLabelValueRefused(t *testing.T) {
	r, c := newAddonReconciler(t, vpnCharts(t), vpnPlatform(), vpnAddon("yes"), testValuesSecret())
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	assert.Equal(t, []string{`Deployment/uplink: label zaentrum.io/vpn: "yes" not allowed (only "true", which assigns the pods to the platform VPN)`},
		a.Status.Plan.Violations)
	_, ok := getDeployment(t, c, "uplink")
	assert.False(t, ok)
	_, ok = getPolicy(t, c)
	assert.False(t, ok)
}

// A spec.vpn the CRD would have refused — served by a copy of the CRD without
// its checks — is refused for every addon, as a broken platform value is, and
// nothing is applied.
func TestAddonInvalidPlatformVPN(t *testing.T) {
	for name, tc := range map[string]struct {
		vpn  zaentrumv1alpha1.VPNSpec
		want string
	}{
		"no networks": {zaentrumv1alpha1.VPNSpec{Proxy: testVPNProxy},
			"platform values: spec.vpn.clusterCIDRs is required when spec.vpn.proxy is set: the cluster's pod and service networks, which a pod assigned to the VPN may still reach directly"},
		"not socks": {zaentrumv1alpha1.VPNSpec{Proxy: "http://vpn.vpn.svc.cluster.local:3128", ClusterCIDRs: []string{"10.42.0.0/16"}},
			`platform values: spec.vpn.proxy "http://vpn.vpn.svc.cluster.local:3128" is not a socks5:// or socks5h:// URL`},
	} {
		t.Run(name, func(t *testing.T) {
			z := testPlatform()
			z.Spec.VPN = tc.vpn
			r, c := newAddonReconciler(t, vpnCharts(t), z, vpnAddon(""), testValuesSecret())
			res, a := reconcileExample(t, r)
			assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
			assert.Equal(t, tc.want, a.Status.Message)
			assert.Equal(t, "PlatformValues", meta.FindStatusCondition(a.Status.Conditions, condTypePlanned).Reason)
			assert.Nil(t, a.Status.Plan)
			assert.Equal(t, addonRequeueRetry, res.RequeueAfter)
			_, ok := getDeployment(t, c, "uplink")
			assert.False(t, ok, "nothing is applied")
		})
	}
}

// A NetworkPolicy of the kill switch's name the addon does not control is a
// collision: the operator never forces its way over it, and applies nothing.
func TestAddonVPNKillSwitchCollision(t *testing.T) {
	handmade := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: vpnPolicyName, Namespace: addonNS}}
	r, c := newAddonReconciler(t, vpnCharts(t), vpnPlatform(), vpnAddon("true"), testValuesSecret(), handmade)
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	assert.Equal(t, []string{"NetworkPolicy/" + vpnPolicyName + ": already exists and is not owned by this addon"}, a.Status.Plan.Violations)
	_, ok := getDeployment(t, c, "uplink")
	assert.False(t, ok)
	p, _ := getPolicy(t, c)
	assert.Empty(t, p.Spec.PolicyTypes, "the hand-made policy is untouched")
}

// The kill switch cannot be applied — the operator may not write
// NetworkPolicies, say: no workload is applied either, so no pod assigned to
// the VPN ever runs without it.
func TestAddonVPNKillSwitchFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(vpnPlatform(), vpnAddon("true"), testValuesSecret()).
		WithStatusSubresource(&zaentrumv1alpha1.ZaentrumAddon{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() == types.ApplyPatchType && obj.GetObjectKind().GroupVersionKind().Kind == "NetworkPolicy" {
					return errors.New("networkpolicies.networking.k8s.io is forbidden")
				}
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
		}).
		Build()
	r := &ZaentrumAddonReconciler{Client: c, Scheme: scheme, APIReader: c, Charts: vpnCharts(t)}

	key := types.NamespacedName{Namespace: addonNS, Name: "example"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.ErrorContains(t, err, "apply NetworkPolicy/"+vpnPolicyName)
	for _, name := range []string{"uplink", "worker"} {
		_, ok := getDeployment(t, c, name)
		assert.False(t, ok, "%s is not applied", name)
	}
	var a zaentrumv1alpha1.ZaentrumAddon
	require.NoError(t, c.Get(context.Background(), key, &a))
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
}
