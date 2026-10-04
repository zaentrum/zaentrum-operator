package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// chinoAPIEnv is chino-api's environment, name to value; an entry from a
// Secret reads as "".
func chinoAPIEnv(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	d := find(t, objs, "Deployment", "chino-api")
	require.NotNil(t, d)
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NotEmpty(t, containers)
	env, _, _ := unstructured.NestedSlice(containers[0].(map[string]interface{}), "env")
	out := map[string]string{}
	for _, e := range env {
		m := e.(map[string]interface{})
		v, _ := m["value"].(string)
		out[m["name"].(string)] = v
	}
	return out
}

// GET /api/config hands every kind of client the id of the public client it
// signs in through, and a client falls back to chino-api's default, "chino",
// only where none is named. The bundled realm has no "chino": a fresh bundled
// install named none for the TV and phone apps, so they asked for "chino" and
// Keycloak answered invalid_client and "Client not found". Bundled, every id
// /api/config names is a client of the realm import, the TV and phone apps'
// with their own grants (realm_test.go).
func TestAPIConfigNamesTheBundledRealmsClients(t *testing.T) {
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"helm install": helmRender(t, nil),
	} {
		env := chinoAPIEnv(t, objs)
		assert.Equal(t, "chino-tv", env["OIDC_CLIENT_ID_TV"], name)
		assert.Equal(t, "chino-mobile", env["OIDC_CLIENT_ID_MOBILE"], name)
		assert.Equal(t, "chino-web", env["OIDC_CLIENT_ID_WEB"], name)
		assert.Equal(t, "zaentrum-web", env["OIDC_CLIENT_ID_PORTAL"], name)

		realm := map[string]bool{}
		for _, c := range realmImport(t, objs)["clients"].([]any) {
			realm[c.(map[string]any)["clientId"].(string)] = true
		}
		for _, key := range []string{"OIDC_CLIENT_ID_TV", "OIDC_CLIENT_ID_MOBILE", "OIDC_CLIENT_ID_WEB", "OIDC_CLIENT_ID_PORTAL"} {
			assert.True(t, realm[env[key]], "%s: %s names %q, which the bundled realm does not have", name, key, env[key])
		}
		assert.False(t, realm["chino"], "the bundled realm has no unified client")
	}

	// The CR may name others; the realm is then the admin's to give them.
	z := base("zaentrum")
	z.Spec.Identity.TVClientID = "living-room"
	z.Spec.Identity.MobileClientID = "pocket"
	env := chinoAPIEnv(t, renderCR(t, z))
	assert.Equal(t, "living-room", env["OIDC_CLIENT_ID_TV"])
	assert.Equal(t, "pocket", env["OIDC_CLIENT_ID_MOBILE"])
}

// With an external provider the CR names the clients its realm has, and a
// kind it names none for is not named at all: chino-api's own default applies
// for it — as on a shared realm that keeps one public client for every app.
// Beta names its web client alone, and renders as before.
func TestAPIConfigNamesAnExternalProvidersClientsOnlyWhenTold(t *testing.T) {
	ext := func() *zaentrumv1alpha1.Zaentrum {
		z := base("zaentrum-beta")
		z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
		z.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
		z.Spec.Identity.ClientID = "media-web"
		return z
	}
	env := chinoAPIEnv(t, renderCR(t, ext()))
	assert.Equal(t, "media-web", env["OIDC_CLIENT_ID_WEB"])
	assert.Equal(t, "media-web", env["OIDC_CLIENT_ID_PORTAL"])
	assert.NotContains(t, env, "OIDC_CLIENT_ID_TV", "nothing named: chino-api's default")
	assert.NotContains(t, env, "OIDC_CLIENT_ID_MOBILE", "nothing named: chino-api's default")

	z := ext()
	z.Spec.Identity.TVClientID = "media-tv"
	z.Spec.Identity.MobileClientID = "media-mobile"
	env = chinoAPIEnv(t, renderCR(t, z))
	assert.Equal(t, "media-tv", env["OIDC_CLIENT_ID_TV"])
	assert.Equal(t, "media-mobile", env["OIDC_CLIENT_ID_MOBILE"])

	objs := helmRender(t, map[string]interface{}{"identity": map[string]interface{}{
		"mode": "external", "issuer": "https://sso.example.org/realms/example", "mobileClientId": "media-mobile",
	}})
	env = chinoAPIEnv(t, objs)
	assert.Equal(t, "media-mobile", env["OIDC_CLIENT_ID_MOBILE"])
	assert.NotContains(t, env, "OIDC_CLIENT_ID_TV")
}

// chino-api finds katalog-manager, where the admin packaging routes go, by the
// name it reads first, KATALOG_MANAGER_URL — never by ANALYZER_BASE_URL, its
// former name, which it reads only where that is unset — and is told no admin
// by subject: ADMIN_SUBJECTS is deprecated, and an empty one let nobody
// through. Every profile, bundled and external.
func TestChinoAPIFindsKatalogManagerByItsName(t *testing.T) {
	ext := base("zaentrum-beta")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	ext.Spec.Databases.Mode = "external"
	ext.Spec.Databases.External.Host = "postgres.example.org"
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"external":     renderCR(t, ext),
		"helm install": helmRender(t, nil),
	} {
		env := chinoAPIEnv(t, objs)
		assert.Equal(t, "http://katalog-manager-api", env["KATALOG_MANAGER_URL"], name)
		assert.NotContains(t, env, "ANALYZER_BASE_URL", "%s: the former name stays unset", name)
		assert.NotContains(t, env, "ADMIN_SUBJECTS", "%s: admins are the realm role's", name)
	}
}
