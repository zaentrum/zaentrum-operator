package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// The Zaentrum CRD ships in four copies, maintained apart, and a cluster only
// ever installs one of them. A field missing from that one is pruned by the API
// server: the operator writes it, and it is gone — status.verification would
// read "never run" forever. So every copy must serve the schema the canonical
// one does. Descriptions are left out of the comparison: some copies carry
// shortened ones on purpose.
var crdCopies = []string{
	"../../bundle/manifests/zaentrum.io_zaentrums.yaml",
	pinnedInstall,
	"../../../deploy/allinone/manifests/10-operator.yaml",
}

const canonicalCRD = "../../config/crd/zaentrum.io_zaentrums.yaml"

// pinnedInstall installs the operator image it pins, and with it the schema
// that operator knows: it moves only when a cluster-admin re-pins it. The
// fields added to the canonical CRD since are served by the next re-pin, which
// splices the canonical CRD into it and empties sinceThePin. Until then the
// pinned operator, which writes none of them, is served exactly its own schema
// — and a field missing from this list, or one the file already serves, fails
// the test.
const pinnedInstall = "../../../deploy/operator-install.yaml"

var sinceThePin = []string{}

// withoutFields is a deep copy of a CRD spec with the given fields — dotted
// paths below openAPIV3Schema, such as "spec.identity.tvClientId" — removed.
func withoutFields(t *testing.T, spec any, fields []string) any {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	versions, _ := out["versions"].([]any)
	for _, v := range versions {
		for _, f := range fields {
			parts := strings.Split(f, ".")
			node, _ := dig(v, "schema", "openAPIV3Schema").(map[string]any)
			for _, p := range parts[:len(parts)-1] {
				node, _ = dig(node, "properties", p).(map[string]any)
			}
			props, _ := node["properties"].(map[string]any)
			require.Contains(t, props, parts[len(parts)-1], "sinceThePin names %s, which the canonical CRD does not serve", f)
			delete(props, parts[len(parts)-1])
		}
	}
	return out
}

func docs(t *testing.T, file string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var out []map[string]any
	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		doc := map[string]any{}
		err := dec.Decode(&doc)
		if err == io.EOF {
			return out
		}
		require.NoError(t, err, file)
		if len(doc) > 0 {
			out = append(out, doc)
		}
	}
}

// zaentrumCRDSpec is the Zaentrum CRD's spec in a file, descriptions removed.
func zaentrumCRDSpec(t *testing.T, file string) any {
	t.Helper()
	for _, d := range docs(t, file) {
		meta, _ := d["metadata"].(map[string]any)
		if d["kind"] == "CustomResourceDefinition" && meta["name"] == "zaentrums.zaentrum.io" {
			return withoutDescriptions(d["spec"])
		}
	}
	t.Fatalf("%s: no zaentrums.zaentrum.io CRD", file)
	return nil
}

func withoutDescriptions(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if k != "description" {
				out[k] = withoutDescriptions(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = withoutDescriptions(val)
		}
		return out
	}
	return v
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, _ := v.(map[string]any)
		v = m[p]
	}
	return v
}

func TestEveryCRDCopyServesTheSameSchema(t *testing.T) {
	want := zaentrumCRDSpec(t, canonicalCRD)

	// The canonical copy carries what verification writes and reads.
	schema := dig(want, "versions")
	versions, _ := schema.([]any)
	require.Len(t, versions, 1)
	props := dig(versions[0], "schema", "openAPIV3Schema", "properties")
	assert.NotNil(t, dig(props, "spec", "properties", "verification", "properties", "enabled"),
		"spec.verification.enabled")
	for _, f := range []string{"result", "trigger", "request", "fingerprint", "version", "startedAt",
		"finishedAt", "job", "passed", "failed", "warned", "skipped", "checks", "message"} {
		assert.NotNil(t, dig(props, "status", "properties", "verification", "properties", f), "status.verification.%s", f)
	}

	for _, file := range crdCopies {
		if file == pinnedInstall {
			assert.Equal(t, withoutFields(t, want, sinceThePin), zaentrumCRDSpec(t, file),
				"%s serves a different Zaentrum schema than %s without the fields added since its pin (sinceThePin)", file, canonicalCRD)
			continue
		}
		assert.Equal(t, want, zaentrumCRDSpec(t, file), "%s serves a different Zaentrum schema than %s", file, canonicalCRD)
	}
}

// managerRules reads the operator's manager ClusterRole rules out of an install
// bundle (the OLM CSV carries them as clusterPermissions).
func managerRules(t *testing.T, file string) []rbacv1.PolicyRule {
	t.Helper()
	for _, d := range docs(t, file) {
		switch d["kind"] {
		case "ClusterRole":
			if dig(d, "metadata", "name") != "zaentrum-operator-manager-role" {
				continue
			}
			var role rbacv1.ClusterRole
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(d, &role))
			return role.Rules
		case "ClusterServiceVersion":
			perms, _ := dig(d, "spec", "install", "spec", "clusterPermissions").([]any)
			require.NotEmpty(t, perms, file)
			var rules []rbacv1.PolicyRule
			for _, p := range perms {
				raw, _ := dig(p, "rules").([]any)
				for _, r := range raw {
					var rule rbacv1.PolicyRule
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(r.(map[string]any), &rule))
					rules = append(rules, rule)
				}
			}
			return rules
		}
	}
	t.Fatalf("%s: no manager ClusterRole", file)
	return nil
}

func ruleHolds(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	in := func(list []string, s string) bool {
		for _, v := range list {
			if v == s || v == "*" {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if in(r.APIGroups, group) && in(r.Resources, resource) && in(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// What verification does with the API, every shipped ClusterRole allows.
func TestEveryClusterRoleHoldsWhatVerificationUses(t *testing.T) {
	needs := []struct{ group, resource, verb string }{
		{"batch", "jobs", "create"}, {"batch", "jobs", "get"}, {"batch", "jobs", "list"}, {"batch", "jobs", "delete"},
		{"", "pods", "get"}, {"", "pods", "list"},
		{"", "secrets", "get"}, {"", "secrets", "create"}, {"", "secrets", "update"}, {"", "secrets", "delete"},
	}
	for _, file := range []string{
		"../../config/rbac/role.yaml",
		"../../bundle/manifests/zaentrum-operator.clusterserviceversion.yaml",
		"../../../deploy/operator-install.yaml",
		"../../../deploy/allinone/manifests/10-operator.yaml",
	} {
		rules := managerRules(t, file)
		for _, n := range needs {
			assert.True(t, ruleHolds(rules, n.group, n.resource, n.verb), "%s: no %s on %q %s", file, n.verb, n.group, n.resource)
		}
	}
}

// What backups need, every ClusterRole shipped with an operator that renders
// them allows: the chart's CronJob is applied, read and, with backups turned
// off, deleted; the restore is a Job like the other runs. So does what a
// certificate from cert-manager needs: the Certificate.
func TestEveryClusterRoleHoldsWhatBackupsUse(t *testing.T) {
	for _, file := range []string{
		"../../config/rbac/role.yaml",
		"../../bundle/manifests/zaentrum-operator.clusterserviceversion.yaml",
		pinnedInstall,
		"../../../deploy/allinone/manifests/10-operator.yaml",
	} {
		rules := managerRules(t, file)
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
			assert.True(t, ruleHolds(rules, "batch", "cronjobs", verb), "%s: no %s on batch cronjobs", file, verb)
		}
		for _, verb := range []string{"create", "get", "list", "delete"} {
			assert.True(t, ruleHolds(rules, "batch", "jobs", verb), "%s: no %s on batch jobs", file, verb)
		}
		assert.True(t, ruleHolds(rules, "", "persistentvolumeclaims", "patch"), file)
		for _, verb := range []string{"get", "create", "patch", "update"} {
			assert.True(t, ruleHolds(rules, "cert-manager.io", "certificates", verb), "%s: no %s on cert-manager.io certificates", file, verb)
		}
	}
}

// What the prune does with the API — list each kind it removes, by label, and
// delete what the render no longer carries — every shipped ClusterRole
// allows, the pinned install's too, so that the next re-pin needs no new rule.
func TestEveryClusterRoleHoldsWhatPruningUses(t *testing.T) {
	resources := map[schema.GroupKind]string{
		{Group: "apps", Kind: "Deployment"}:                       "deployments",
		{Kind: "Service"}:                                         "services",
		{Kind: "ServiceAccount"}:                                  "serviceaccounts",
		{Group: "route.openshift.io", Kind: "Route"}:              "routes",
		{Group: "networking.k8s.io", Kind: "Ingress"}:             "ingresses",
		{Group: "batch", Kind: "CronJob"}:                         "cronjobs",
		{Group: "rbac.authorization.k8s.io", Kind: "Role"}:        "roles",
		{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}: "rolebindings",
		{Group: "cert-manager.io", Kind: "Certificate"}:           "certificates",
	}
	require.Len(t, templates.PruneKinds, len(resources), "a kind the prune removes, and no rule checked for it here")
	for _, file := range []string{
		"../../config/rbac/role.yaml",
		"../../bundle/manifests/zaentrum-operator.clusterserviceversion.yaml",
		pinnedInstall,
		"../../../deploy/allinone/manifests/10-operator.yaml",
	} {
		rules := managerRules(t, file)
		for _, gvk := range templates.PruneKinds {
			resource, ok := resources[gvk.GroupKind()]
			require.True(t, ok, "%s: which resource?", gvk)
			for _, verb := range []string{"list", "delete"} {
				assert.True(t, ruleHolds(rules, gvk.Group, resource, verb), "%s: no %s on %q %s", file, verb, gvk.Group, resource)
			}
		}
	}
}

// What the addon controller does with each kind it applies for an addon — the
// chart's, and the NetworkPolicy that is the kill switch of the pods it assigns
// to the platform VPN — every shipped ClusterRole allows: get (the collision
// check), list (the inventory, of every addon on every pass, whether it uses
// the VPN or not), create and patch (server-side apply) and delete (prune). The
// pinned install's too: an operator re-pinned without the NetworkPolicy rule
// would have every addon's inventory refused, so the pinned install holds the
// rule ahead of spec.vpn, which its CRD serves from that re-pin on
// (sinceThePin).
func TestEveryClusterRoleHoldsWhatAddonsUse(t *testing.T) {
	resources := map[schema.GroupKind]string{
		{Kind: "ServiceAccount"}:                            "serviceaccounts",
		{Kind: "Secret"}:                                    "secrets",
		{Kind: "ConfigMap"}:                                 "configmaps",
		{Kind: "PersistentVolumeClaim"}:                     "persistentvolumeclaims",
		{Kind: "Service"}:                                   "services",
		{Group: "networking.k8s.io", Kind: "NetworkPolicy"}: "networkpolicies",
		{Group: "apps", Kind: "Deployment"}:                 "deployments",
		{Group: "batch", Kind: "Job"}:                       "jobs",
	}
	require.Len(t, addon.AppliedKinds, len(resources), "a kind the addon controller applies, and no rule checked for it here")
	for _, file := range []string{
		"../../config/rbac/role.yaml",
		"../../bundle/manifests/zaentrum-operator.clusterserviceversion.yaml",
		pinnedInstall,
		"../../../deploy/allinone/manifests/10-operator.yaml",
	} {
		rules := managerRules(t, file)
		for _, gvk := range addon.AppliedKinds {
			resource, ok := resources[gvk.GroupKind()]
			require.True(t, ok, "%s: which resource?", gvk)
			for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
				assert.True(t, ruleHolds(rules, gvk.Group, resource, verb), "%s: no %s on %q %s", file, verb, gvk.Group, resource)
			}
		}
	}
}

// spec.vpn is checked where it is written, and the operator, which holds
// whatever served the Zaentrum to the same rules (addon.PlatformVPN), agrees
// with the CRD on each example here: a proxy is a socks5:// or socks5h:// URL
// of a host and a port alone, a cluster CIDR is one net.ParseCIDR reads — the
// API server's format cidr — with a prefix length of at least 1, and a proxy
// needs a CIDR (a CEL rule, which only a cluster runs). An IPv4-mapped IPv6
// network is the one CIDR only the operator refuses.
func TestVPNSchemaChecksWhatTheOperatorChecks(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	vpn := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties", "vpn")
	require.NotNil(t, vpn, "spec.vpn")

	pattern, _ := dig(vpn, "properties", "proxy", "pattern").(string)
	require.NotEmpty(t, pattern)
	proxy := regexp.MustCompile(pattern)
	operatorTakes := func(spec zaentrumv1alpha1.VPNSpec) bool {
		_, err := addon.PlatformVPN(spec)
		return err == nil
	}
	for _, ok := range []string{
		"socks5h://vpn.vpn.svc.cluster.local:1080", "socks5://vpn:1080", "socks5h://vpn.vpn.svc.cluster.local.:1080",
		"socks5h://10.43.0.10:1080", "socks5h://[fd00:10:43::10]:1080", "socks5h://[::1]:1", "socks5://VPN.example.org:65535",
	} {
		assert.True(t, proxy.MatchString(ok), "the CRD refuses a proxy the operator takes: %q", ok)
		assert.True(t, operatorTakes(zaentrumv1alpha1.VPNSpec{Proxy: ok, ClusterCIDRs: []string{"10.42.0.0/16"}}), "the operator refuses %q", ok)
	}
	for _, bad := range []string{
		"http://vpn:1080", "socks4://vpn:1080", "SOCKS5://vpn:1080", "socks5:vpn:1080", "socks5://vpn", "socks5://vpn:",
		"socks5://:1080", "socks5://vpn:0", "socks5://vpn:01080", "socks5://vpn:65536", "socks5://vpn:99999",
		"socks5://vpn:1080/", "socks5://vpn:1080/x", "socks5://vpn:1080?x=1", "socks5://vpn:1080#x",
		"socks5://user:secret@vpn:1080", "socks5://user@vpn:1080", "socks5h://fd00::10:1080", "socks5h://[fd00::10%25eth0]:1080",
		"socks5h://[1.2.3.4]:1080", "socks5://vpn_x:1080", "socks5://-vpn:1080", "socks5://vpn..x:1080", " socks5://vpn:1080",
	} {
		assert.False(t, proxy.MatchString(bad), "the CRD takes a proxy the operator refuses: %q", bad)
		assert.False(t, operatorTakes(zaentrumv1alpha1.VPNSpec{Proxy: bad, ClusterCIDRs: []string{"10.42.0.0/16"}}), "the operator takes %q", bad)
	}

	items := dig(vpn, "properties", "clusterCIDRs", "items")
	assert.Equal(t, "cidr", dig(items, "format"))
	itemPattern, _ := dig(items, "pattern").(string)
	require.NotEmpty(t, itemPattern)
	cidr := regexp.MustCompile(itemPattern)
	crdTakes := func(c string) bool {
		_, _, err := net.ParseCIDR(c)
		return err == nil && cidr.MatchString(c)
	}
	for _, ok := range []string{"10.42.0.0/16", "10.43.0.0/16", "10.42.0.1/16", "fd00:10:42::/56", "192.168.0.0/24", "10.0.0.1/32", "fd00::1/128"} {
		assert.True(t, crdTakes(ok), "the CRD refuses a CIDR the operator takes: %q", ok)
		assert.True(t, operatorTakes(zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{ok}}), "the operator refuses %q", ok)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.42.0.0", "10.42.0.0/33", "fd00::/129", "10.0.0.0/016", "010.42.0.0/16",
		"10.42.0.0/16 ", "fe80::%eth0/64", "cluster"} {
		assert.False(t, crdTakes(bad), "the CRD takes a CIDR the operator refuses: %q", bad)
		assert.False(t, operatorTakes(zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{bad}}), "the operator takes %q", bad)
	}
	assert.True(t, crdTakes("::ffff:10.42.0.0/112"))
	assert.False(t, operatorTakes(zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{"::ffff:10.42.0.0/112"}}))

	assert.Equal(t, []any{map[string]any{
		"rule":    "!has(self.proxy) || size(self.proxy) == 0 || (has(self.clusterCIDRs) && size(self.clusterCIDRs) > 0)",
		"message": "clusterCIDRs is required when proxy is set",
	}}, dig(vpn, "x-kubernetes-validations"), "a proxy needs a CIDR")
	assert.False(t, operatorTakes(zaentrumv1alpha1.VPNSpec{Proxy: "socks5h://vpn:1080"}), "nor does the operator take a proxy without one")
	assert.True(t, operatorTakes(zaentrumv1alpha1.VPNSpec{}), "no VPN")
	assert.True(t, operatorTakes(zaentrumv1alpha1.VPNSpec{ClusterCIDRs: []string{"10.42.0.0/16"}}), "networks without a proxy: no VPN either")
}

// spec.pipeline.ladder is checked where it is written: the CRD's pattern takes
// the ladders the transcoder parses — rungs source or NNNp, each with an
// optional codec and maxrate in either order — and refuses what the transcoder
// would refuse at its start, where a typo becomes a crash loop. The extras'
// ladder, spec.pipeline.extraLadder, the transcoder parses alike, and the CRD
// checks it alike.
func TestLadderPatternTakesWhatTheTranscoderTakes(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	pattern, _ := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties",
		"pipeline", "properties", "ladder", "pattern").(string)
	require.NotEmpty(t, pattern)
	extra, _ := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties",
		"pipeline", "properties", "extraLadder", "pattern").(string)
	assert.Equal(t, pattern, extra, "spec.pipeline.extraLadder is checked as spec.pipeline.ladder is")
	re := regexp.MustCompile(pattern)
	for _, ok := range []string{"source", "source,720p", "source, 720p", "source,720p,480p",
		"720p:h264:2500k", "source:hevc", "1080p:3M:h264", "source,720p:h264:2.5M", "2160p:hevc:14m",
		"720p:h264,480p:h264"} {
		assert.True(t, re.MatchString(ok), "the CRD refuses a ladder the transcoder takes: %q", ok)
	}
	for _, bad := range []string{"720", "1080i", "source,,720p", "source;720p", "720p:av1", "720p:", "source,",
		"src", "720p:2.5"} {
		assert.False(t, re.MatchString(bad), "the CRD takes a ladder the transcoder refuses: %q", bad)
	}
}
