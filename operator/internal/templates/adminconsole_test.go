package templates

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// keycloakEnv is the bundled Keycloak container's env, by name.
func keycloakEnv(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	u := find(t, objs, "Deployment", "keycloak")
	require.NotNil(t, u)
	var dep appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dep))
	out := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func realmJobEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum) map[string]string {
	t.Helper()
	_, hooks := SplitHooks(renderCR(t, z))
	u := RealmJob(hooks)
	require.NotNil(t, u)
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &job))
	out := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

// consoleProfiles are a plain Ingress and OpenShift Routes, the console off and on.
func consoleProfiles(expose bool) map[string]*zaentrumv1alpha1.Zaentrum {
	ingress := base("zaentrum")
	routes := demoCR("zaentrum-demo")
	ingress.Spec.Identity.ExposeAdminConsole = expose
	routes.Spec.Identity.ExposeAdminConsole = expose
	return map[string]*zaentrumv1alpha1.Zaentrum{"ingress": ingress, "routes": routes}
}

// By default the public host serves only what people sign in through: the
// admin console and the admin API answer through a port-forward, where they
// also sign in.
func TestAdminConsoleIsNotOnThePublicHost(t *testing.T) {
	for name, z := range consoleProfiles(false) {
		t.Run(name, func(t *testing.T) {
			objs := renderCR(t, z)
			routes := routesOf(t, objs)
			host := z.Spec.Hostname
			if host == "" {
				host = "zaentrum.localhost"
			}
			for path, want := range map[string]string{
				"/auth/realms/zaentrum/protocol/openid-connect/auth":     "keycloak",
				"/auth/realms/zaentrum/.well-known/openid-configuration": "keycloak",
				"/auth/realms/zaentrum/account":                          "keycloak",
				"/auth/realms/zaentrum/device":                           "keycloak",
				"/auth/resources/x1y2z/login/zaentrum/css/zaentrum.css":  "keycloak",
				"/auth/callback": "chino-web",
			} {
				assert.Equal(t, want, serving(routes, host, path), path)
			}
			for _, path := range []string{"/auth/admin/", "/auth/admin/master/console/",
				"/auth/admin/realms/zaentrum/users", "/auth/", "/auth/realmsX"} {
				assert.NotEqual(t, "keycloak", serving(routes, host, path), "%s reaches Keycloak from the public host", path)
			}

			assert.Equal(t, "http://localhost:8080/auth", keycloakEnv(t, objs)["KC_HOSTNAME_ADMIN"],
				"the console's links and sign-in point at the port-forward")
			assert.Equal(t, "http://localhost:8080/auth", realmJobEnv(t, z)["MASTER_FRONTEND_URL"],
				"the master realm signs the console in there too")
		})
	}
}

// identity.exposeAdminConsole publishes /auth whole, as before.
func TestAdminConsoleCanBePublished(t *testing.T) {
	for name, z := range consoleProfiles(true) {
		t.Run(name, func(t *testing.T) {
			objs := renderCR(t, z)
			routes := routesOf(t, objs)
			host := z.Spec.Hostname
			if host == "" {
				host = "zaentrum.localhost"
			}
			for _, path := range []string{"/auth/admin/master/console/", "/auth/realms/zaentrum/account", "/auth/resources/x1y2z/x.css"} {
				assert.Equal(t, "keycloak", serving(routes, host, path), path)
			}
			assert.Equal(t, "chino-web", serving(routes, host, "/auth/callback"), "the SPA's callback stays the SPA's")
			_, set := keycloakEnv(t, objs)["KC_HOSTNAME_ADMIN"]
			assert.False(t, set, "the console lives on the public host")
			env := realmJobEnv(t, z)
			require.Contains(t, env, "MASTER_FRONTEND_URL")
			assert.Empty(t, env["MASTER_FRONTEND_URL"], "the master realm's frontend URL is unset again")
		})
	}
}

// Off and on render the same Routes, only their paths differ: the operator
// prunes no object it stopped rendering, so a Route only the published console
// had would stay and keep it public after it is turned off.
func TestAdminConsoleTogglesTheSameRoutes(t *testing.T) {
	names := func(expose bool) []string {
		z := demoCR("zaentrum-demo")
		z.Spec.Identity.ExposeAdminConsole = expose
		var out []string
		for _, o := range renderCR(t, z) {
			if o.GetKind() == "Route" || o.GetKind() == "Ingress" {
				out = append(out, o.GetKind()+"/"+o.GetName())
			}
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, names(true), names(false))
}

// With external identity there is no Keycloak to route to.
func TestNoKeycloakRouteWithExternalIdentity(t *testing.T) {
	z := demoCR("zaentrum-demo")
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	for _, r := range routesOf(t, renderCR(t, z)) {
		assert.NotEqual(t, "keycloak", r.service, "%s%s", r.host, r.path)
	}
}
