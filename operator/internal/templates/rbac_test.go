package templates

import (
	"bytes"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// operatorRules reads the operator's manager ClusterRole from config/rbac.
func operatorRules(t *testing.T) []rbacv1.PolicyRule {
	t.Helper()
	data, err := os.ReadFile("../../config/rbac/role.yaml")
	require.NoError(t, err)
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var role rbacv1.ClusterRole
		err := dec.Decode(&role)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if role.Name == "zaentrum-operator-manager-role" {
			return role.Rules
		}
	}
	t.Fatal("config/rbac/role.yaml has no zaentrum-operator-manager-role")
	return nil
}

func holds(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	has := func(list []string, v string) bool {
		for _, s := range list {
			if s == v || s == "*" {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if has(r.APIGroups, group) && has(r.Resources, resource) && has(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// verbsFor returns the sorted verbs a Role grants on group/resource.
func verbsFor(role rbacv1.Role, group, resource string) []string {
	var verbs []string
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				if g == group && res == resource {
					verbs = append(verbs, r.Verbs...)
				}
			}
		}
	}
	sort.Strings(verbs)
	return verbs
}

func renderedRoles(t *testing.T, z *zaentrumv1alpha1.Zaentrum) []rbacv1.Role {
	t.Helper()
	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	var roles []rbacv1.Role
	for _, o := range objs {
		if o.GetKind() != "Role" {
			continue
		}
		var role rbacv1.Role
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &role))
		roles = append(roles, role)
	}
	return roles
}

// The operator applies the chart's Roles, and RBAC refuses a Role granting
// anything its creator does not hold. Every rule a rendered Role grants must
// be in the operator's ClusterRole, or the platform apply fails in a fresh
// namespace.
func TestOperatorHoldsWhatTheChartGrants(t *testing.T) {
	held := operatorRules(t)
	shared := base("zaentrum-beta")
	shared.Spec.EventStreaming.Mode = "external"
	shared.Spec.EventStreaming.Bootstrap = "broker.events.svc:9093"
	shared.Spec.Identity.Mode = "external"
	shared.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	shared.Spec.Features.Pipeline = true

	for name, z := range map[string]*zaentrumv1alpha1.Zaentrum{
		"self-host": base("zaentrum"), "demo": demoCR("zaentrum-demo"), "shared": shared,
	} {
		roles := renderedRoles(t, z)
		require.NotEmpty(t, roles, name)
		for _, role := range roles {
			for _, rule := range role.Rules {
				for _, group := range rule.APIGroups {
					for _, resource := range rule.Resources {
						for _, verb := range rule.Verbs {
							assert.True(t, holds(held, group, resource, verb),
								"%s: Role/%s grants %s on %q %s, which the operator does not hold", name, role.Name, verb, group, resource)
						}
					}
				}
			}
		}
	}
}

// portal-api manages addon charts, and writes addon secret inputs it can
// never read back.
func TestPortalAPIRoleManagesAddons(t *testing.T) {
	var portal *rbacv1.Role
	for _, role := range renderedRoles(t, base("zaentrum")) {
		if role.Name == "portal-api" {
			portal = role.DeepCopy()
		}
	}
	require.NotNil(t, portal)
	assert.Equal(t, []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		verbsFor(*portal, "zaentrum.io", "zaentrumaddons"))
	assert.Equal(t, []string{"create"}, verbsFor(*portal, "", "secrets"),
		"create only: no get/list/watch, and no patch/delete")
}
