package controller

import (
	"bytes"
	"encoding/json"
	"io"
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

// spec.pipeline.ladder is checked where it is written: the CRD's pattern takes
// the ladders the transcoder parses — rungs source or NNNp, each with an
// optional codec and maxrate in either order — and refuses what the transcoder
// would refuse at its start, where a typo becomes a crash loop.
func TestLadderPatternTakesWhatTheTranscoderTakes(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	pattern, _ := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties",
		"pipeline", "properties", "ladder", "pattern").(string)
	require.NotEmpty(t, pattern)
	re := regexp.MustCompile(pattern)
	for _, ok := range []string{"source", "source,720p", "source, 720p", "source,720p,480p",
		"720p:h264:2500k", "source:hevc", "1080p:3M:h264", "source,720p:h264:2.5M", "2160p:hevc:14m"} {
		assert.True(t, re.MatchString(ok), "the CRD refuses a ladder the transcoder takes: %q", ok)
	}
	for _, bad := range []string{"720", "1080i", "source,,720p", "source;720p", "720p:av1", "720p:", "source,",
		"src", "720p:2.5"} {
		assert.False(t, re.MatchString(bad), "the CRD takes a ladder the transcoder refuses: %q", bad)
	}
}
