package addon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

const testProxy = "socks5h://vpn.vpn.svc.cluster.local:1080"

var testCIDRs = []string{"10.42.0.0/16", "10.43.0.0/16"}

// vpnWorkload is a Deployment or a Job named uplink whose pod template carries
// the given labels besides its own (flow-style YAML, "" for none).
func vpnWorkload(kind, podLabels string) string {
	if podLabels != "" {
		podLabels = ", " + podLabels
	}
	head, selector, restart := "apiVersion: apps/v1\nkind: Deployment\n", "  selector: {matchLabels: {app: uplink}}\n", ""
	if kind == "Job" {
		head, selector, restart = "apiVersion: batch/v1\nkind: Job\n", "", "restartPolicy: Never, "
	}
	return head + `metadata: {name: uplink}
spec:
` + selector + `  template:
    metadata: {labels: {app: uplink` + podLabels + `}}
    spec: {` + restart + `containers: [{name: app, image: app}]}
`
}

func toPolicy(t *testing.T, o *unstructured.Unstructured) *networkingv1.NetworkPolicy {
	t.Helper()
	var p networkingv1.NetworkPolicy
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &p))
	return &p
}

// A Deployment or a Job whose pod template carries zaentrum.io/vpn: "true" gets
// its kill switch: one NetworkPolicy, of policy type Egress only, that lets the
// addon's pods assigned to the VPN connect to the cluster's networks alone, on
// every port and protocol.
func TestVPNPolicyForAssignedPods(t *testing.T) {
	for _, kind := range []string{"Deployment", "Job"} {
		t.Run(kind, func(t *testing.T) {
			objs := objects(t, "apiVersion: v1\nkind: Service\nmetadata: {name: worker}\n---\n"+vpnWorkload(kind, `zaentrum.io/vpn: "true"`))
			o := VPNPolicy(objs, "example", testNamespace, testCIDRs)
			require.NotNil(t, o)
			assert.Equal(t, "networking.k8s.io/v1", o.GetAPIVersion())
			assert.Equal(t, "NetworkPolicy", o.GetKind())
			assert.Equal(t, "zaentrum-addon-example-vpn", o.GetName())
			assert.Equal(t, testNamespace, o.GetNamespace())

			p := toPolicy(t, o)
			assert.Equal(t, metav1.LabelSelector{MatchLabels: map[string]string{
				"zaentrum.io/addon": "example", "zaentrum.io/vpn": "true",
			}}, p.Spec.PodSelector)
			assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, p.Spec.PolicyTypes, "egress only: ingress is not the kill switch's")
			assert.Empty(t, p.Spec.Ingress)
			assert.Equal(t, []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: "10.42.0.0/16"}},
				{IPBlock: &networkingv1.IPBlock{CIDR: "10.43.0.0/16"}},
			}}}, p.Spec.Egress, "the cluster's networks alone, every port and protocol (no ports)")
		})
	}
}

// No pod template assigned to the VPN, no policy: not for unlabelled
// workloads, nor for the label on an object's own metadata, nor for a value
// other than "true" (which the guardrails refuse).
func TestVPNPolicyNotRenderedWithoutAssignedPods(t *testing.T) {
	for name, manifest := range map[string]string{
		"no label":        vpnWorkload("Deployment", ""),
		"on the metadata": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: uplink, labels: {zaentrum.io/vpn: \"true\"}}\nspec:\n  selector: {matchLabels: {app: uplink}}\n  template:\n    metadata: {labels: {app: uplink}}\n    spec: {containers: [{name: app, image: app}]}\n",
		"not a workload":  "apiVersion: v1\nkind: Service\nmetadata: {name: uplink, labels: {zaentrum.io/vpn: \"true\"}}\n",
		"another value":   vpnWorkload("Deployment", `zaentrum.io/vpn: "false"`),
		"unquoted":        vpnWorkload("Deployment", `zaentrum.io/vpn: true`),
	} {
		assert.Nil(t, VPNPolicy(objects(t, manifest), "example", testNamespace, testCIDRs), name)
	}
	assert.Nil(t, VPNPolicy(renderExample(t, nil).Objects, "example", testNamespace, testCIDRs), "the example chart")
}

// The policy selects the addon's own pods assigned to the VPN as the operator
// applies them (Decorate labels every pod template with its addon), and never
// another addon's, the platform's, or the addon's pods outside the VPN.
func TestVPNPolicyScopedToTheAddon(t *testing.T) {
	objs := objects(t, vpnWorkload("Deployment", `zaentrum.io/vpn: "true"`)+"---\n"+workload(`{containers: [{name: app, image: app}]}`))
	Decorate(objs, Decoration{Name: "example", Namespace: testNamespace, PartOf: "p", Checksum: "c"})
	other := objects(t, vpnWorkload("Deployment", `zaentrum.io/vpn: "true"`))
	Decorate(other, Decoration{Name: "other", Namespace: testNamespace, PartOf: "p", Checksum: "c"})

	p := toPolicy(t, VPNPolicy(objs, "example", testNamespace, testCIDRs))
	selector, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
	require.NoError(t, err)
	podLabels := func(o *unstructured.Unstructured) labels.Set {
		m, _, _ := unstructured.NestedStringMap(o.Object, "spec", "template", "metadata", "labels")
		return m
	}
	assert.True(t, selector.Matches(podLabels(findObject(objs, "Deployment", "uplink"))), "the addon's pods assigned to the VPN")
	assert.False(t, selector.Matches(podLabels(findObject(objs, "Deployment", "worker"))), "the addon's other pods")
	assert.False(t, selector.Matches(podLabels(other[0])), "another addon's pods assigned to the VPN")
	assert.False(t, selector.Matches(labels.Set{"app": "chino-api", "zaentrum.io/vpn": "true"}), "a pod without the addon's label")
}

// Without a cluster network to allow, the kill switch refuses every
// connection: it carries no egress rule, never one without peers, which would
// allow every destination.
func TestVPNPolicyWithoutNetworksRefusesEverything(t *testing.T) {
	o := VPNPolicy(objects(t, vpnWorkload("Deployment", `zaentrum.io/vpn: "true"`)), "example", testNamespace, nil)
	require.NotNil(t, o)
	_, found, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "egress")
	assert.False(t, found, "no egress rule at all")
	p := toPolicy(t, o)
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, p.Spec.PolicyTypes)
}

// The guardrails: zaentrum.io/vpn on a pod template takes "true" alone, and
// only on a platform with a VPN; charts still render no NetworkPolicy.
func TestGuardVPNLabel(t *testing.T) {
	guardVPN := func(manifest, proxy string) []string {
		return Violations(objects(t, manifest), GuardInput{Namespace: testNamespace, MediaClaim: "media", VPNProxy: proxy})
	}
	for _, kind := range []string{"Deployment", "Job"} {
		id := kind + "/uplink"
		for value, want := range map[string]string{
			`"false"`: id + `: label zaentrum.io/vpn: "false" not allowed (only "true", which assigns the pods to the platform VPN)`,
			`""`:      id + `: label zaentrum.io/vpn: "" not allowed (only "true", which assigns the pods to the platform VPN)`,
			`"yes"`:   id + `: label zaentrum.io/vpn: "yes" not allowed (only "true", which assigns the pods to the platform VPN)`,
			`"True"`:  id + `: label zaentrum.io/vpn: "True" not allowed (only "true", which assigns the pods to the platform VPN)`,
			`true`:    id + `: label zaentrum.io/vpn: true not allowed (only the string "true", which assigns the pods to the platform VPN)`,
		} {
			assert.Contains(t, guardVPN(vpnWorkload(kind, "zaentrum.io/vpn: "+value), testProxy), want, "%s %s", kind, value)
		}
		assert.Equal(t, []string{id + `: label zaentrum.io/vpn: "true" assigns the pods to the platform VPN, but the platform has none (spec.vpn.proxy): they would have no internet`},
			guardVPN(vpnWorkload(kind, `zaentrum.io/vpn: "true"`), ""), "%s without a VPN", kind)
		assert.Empty(t, guardVPN(vpnWorkload(kind, `zaentrum.io/vpn: "true"`), testProxy), "%s assigned to the platform's VPN", kind)
		assert.Empty(t, guardVPN(vpnWorkload(kind, ""), ""), "%s outside the VPN, on a platform without one", kind)
	}
	assert.Empty(t, guardVPN("apiVersion: v1\nkind: Service\nmetadata: {name: uplink, labels: {zaentrum.io/vpn: \"no\"}}\n", ""),
		"the label means nothing but on a pod template")

	policy := "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: allow-all}\nspec: {podSelector: {}, policyTypes: [Egress], egress: [{}]}\n"
	assert.Equal(t, []string{"NetworkPolicy/allow-all: kind not allowed"}, guardVPN(policy, testProxy), "a chart renders no NetworkPolicy")
	for _, gvk := range AllowedKinds {
		assert.NotEqual(t, "NetworkPolicy", gvk.Kind, "AllowedKinds holds no NetworkPolicy")
	}
}

// The operator applies its own kinds with the chart's — the kill switch before
// the workloads it guards — lists them back and checks their names.
func TestAppliedKinds(t *testing.T) {
	assert.ElementsMatch(t, append(append([]schema.GroupVersionKind{}, AllowedKinds...), OperatorKinds...), AppliedKinds,
		"AppliedKinds is AllowedKinds and OperatorKinds")
	var chart []schema.GroupVersionKind
	for _, gvk := range AppliedKinds {
		if _, ok := allowedKind(gvk.Kind); ok {
			chart = append(chart, gvk)
		}
	}
	assert.Equal(t, AllowedKinds, chart, "the chart's objects apply in the order they did")

	objs := objects(t, `apiVersion: batch/v1
kind: Job
metadata: {name: j}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: zaentrum-addon-example-vpn}
---
apiVersion: v1
kind: Service
metadata: {name: s}
`)
	ApplyOrder(objs)
	var keys []string
	for _, o := range objs {
		keys = append(keys, Key(o))
	}
	assert.Equal(t, []string{"Service/s", "NetworkPolicy/zaentrum-addon-example-vpn", "Deployment/d", "Job/j"}, keys,
		"no pod assigned to the VPN starts before its kill switch")

	// A kill switch whose name is taken is a collision like any other.
	var looked []string
	violations, err := Collisions(objs, "addon-uid", func(o *unstructured.Unstructured) (bool, *metav1.OwnerReference, error) {
		looked = append(looked, Key(o))
		return o.GetKind() == "NetworkPolicy", nil, nil
	})
	require.NoError(t, err)
	assert.Contains(t, looked, "NetworkPolicy/zaentrum-addon-example-vpn")
	assert.Equal(t, []string{"NetworkPolicy/zaentrum-addon-example-vpn: already exists and is not owned by this addon"}, violations)

	_, err = Collisions(objs, types.UID("addon-uid"), func(*unstructured.Unstructured) (bool, *metav1.OwnerReference, error) {
		return false, nil, errors.New("api down")
	})
	assert.EqualError(t, err, "api down")
}

// requireVPN is what operator/README.md has a chart whose pods must never run
// outside the VPN put under "zaentrum" in its values.schema.json.
const requireVPN = `{"required": ["vpn"], "properties": {"vpn": {"required": ["proxy"], "properties": {"proxy": {"type": "string", "minLength": 1}}}}}`

// Such a chart renders on a platform with a VPN, and is refused on one without
// a VPN and by an operator from before the VPN, which hands addons no
// zaentrum.vpn at all — and renders no kill switch.
func TestChartCanRequireTheVPN(t *testing.T) {
	chrt := exampleChart(t)
	var schema, zaentrum map[string]interface{}
	require.NoError(t, json.Unmarshal(chrt.Schema, &schema))
	require.NoError(t, json.Unmarshal([]byte(requireVPN), &zaentrum))
	schema["properties"].(map[string]interface{})["zaentrum"] = zaentrum
	raw, err := json.Marshal(schema)
	require.NoError(t, err)
	chrt.Schema = raw

	render := func(platform map[string]interface{}) []string {
		user := map[string]interface{}{"auth": map[string]interface{}{"token": "t0ken"}}
		vals := LayerValues(map[string]string{"auth.signingKey": "c2lnbmluZw=="}, user, platform)
		_, errs := Render(RenderInput{Chart: chrt, Name: "example", Namespace: testNamespace, Values: vals})
		return errs
	}
	with := platformValues()
	with["vpn"] = map[string]interface{}{"proxy": testProxy}
	assert.Empty(t, render(with), "a platform with a VPN")
	assert.Equal(t, []string{"zaentrum.vpn.proxy: String length must be greater than or equal to 1"}, render(platformValues()),
		"a platform without one")
	before := platformValues()
	delete(before, "vpn")
	assert.Equal(t, []string{"zaentrum: vpn is required"}, render(before), "an operator from before the VPN")
}

// spec.vpn as the operator holds it, whatever served it: a socks5:// or
// socks5h:// proxy of a host and a port alone, CIDRs that parse and are not
// every address, at least one with a proxy. The networks come back as an
// ipBlock takes them, each once, in the Zaentrum's order.
func TestPlatformVPN(t *testing.T) {
	vpn, err := PlatformVPN(zaentrumv1alpha1.VPNSpec{
		Proxy:        testProxy,
		ClusterCIDRs: []string{"10.43.0.0/16", "10.42.0.1/16", "10.42.0.0/16", "fd00:10:42::1/56"},
	})
	require.NoError(t, err)
	assert.Equal(t, VPN{Proxy: testProxy, ClusterCIDRs: []string{"10.43.0.0/16", "10.42.0.0/16", "fd00:10:42::/56"}}, vpn)

	none, err := PlatformVPN(zaentrumv1alpha1.VPNSpec{})
	require.NoError(t, err)
	assert.Equal(t, VPN{}, none, "no VPN")

	for name, tc := range map[string]struct {
		spec zaentrumv1alpha1.VPNSpec
		want string
	}{
		"no networks": {zaentrumv1alpha1.VPNSpec{Proxy: testProxy},
			"spec.vpn.clusterCIDRs is required when spec.vpn.proxy is set: the cluster's pod and service networks, which a pod assigned to the VPN may still reach directly"},
		"not a CIDR": {zaentrumv1alpha1.VPNSpec{Proxy: testProxy, ClusterCIDRs: []string{"10.42.0.0/16", "10.43.0.0"}},
			`spec.vpn.clusterCIDRs[1]: "10.43.0.0" is not a CIDR`},
		"every address": {zaentrumv1alpha1.VPNSpec{Proxy: testProxy, ClusterCIDRs: []string{"0.0.0.0/0"}},
			`spec.vpn.clusterCIDRs[0]: "0.0.0.0/0" is every address, not the cluster's networks: a pod assigned to the VPN could reach the internet around it`},
		"every IPv6 address": {zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{"::/0"}},
			`spec.vpn.clusterCIDRs[0]: "::/0" is every address, not the cluster's networks: a pod assigned to the VPN could reach the internet around it`},
		"IPv4-mapped": {zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{"::ffff:10.42.0.0/112"}},
			`spec.vpn.clusterCIDRs[0]: "::ffff:10.42.0.0/112" is an IPv4-mapped IPv6 network: name it as IPv4`},
		"http": {zaentrumv1alpha1.VPNSpec{Proxy: "http://vpn:3128", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "http://vpn:3128" is not a socks5:// or socks5h:// URL`},
		"no port": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://vpn", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://vpn" needs a port, 1-65535`},
		"port out of range": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://vpn:65536", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://vpn:65536" needs a port, 1-65535`},
		"no host": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://:1080", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://:1080" needs a host`},
		"a path": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://vpn:1080/", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://vpn:1080/" must be socks5://<host>:<port> and nothing else`},
		"unbracketed IPv6": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5h://fd00::10:1080", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5h://fd00::10:1080" names an IPv6 address outside brackets`},
		"not a host name": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://vpn_x:1080", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://vpn_x:1080" names a host that is neither a DNS name nor an IP address`},
		// The password never reaches a status message.
		"credentials": {zaentrumv1alpha1.VPNSpec{Proxy: "socks5://user:s3cret@vpn:1080", ClusterCIDRs: testCIDRs},
			`spec.vpn.proxy "socks5://user:xxxxx@vpn:1080" must not carry credentials: every addon is handed it`},
	} {
		_, err := PlatformVPN(tc.spec)
		require.Error(t, err, name)
		assert.Equal(t, tc.want, err.Error(), name)
		assert.NotContains(t, err.Error(), "s3cret", name)
	}
}
