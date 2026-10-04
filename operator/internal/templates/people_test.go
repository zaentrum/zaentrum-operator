package templates

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The People page (zaentrum-portal: the People page, its invites and account
// deletion) manages the bundled realm's people through one client, whose
// service account may view, query and manage users and do nothing else; a
// person's rating cap is a user attribute only an admin changes, which the
// viewers' access tokens carry as the claim max_rating. Bundled identity
// only: an external provider's people are its own.

// userProfile is the realm's user profile, as the import's user-profile
// component carries it: one JSON document, as a string.
func userProfile(t *testing.T, realm map[string]any) string {
	t.Helper()
	components, _ := realm["components"].(map[string]any)
	require.NotNil(t, components, "the realm import has no components")
	list, _ := components["org.keycloak.userprofile.UserProfileProvider"].([]any)
	require.Len(t, list, 1, "one user profile")
	c := list[0].(map[string]any)
	assert.Equal(t, "declarative-user-profile", c["providerId"])
	config, _ := c["config"].(map[string]any)
	values, _ := config["kc.user.profile.config"].([]any)
	require.Len(t, values, 1, "the profile is one value")
	s, ok := values[0].(string)
	require.True(t, ok, "the profile is a JSON string")
	return s
}

// profileAttribute is one attribute of the realm's user profile.
func profileAttribute(t *testing.T, realm map[string]any, name string) map[string]any {
	t.Helper()
	var profile struct {
		Attributes []map[string]any `json:"attributes"`
	}
	require.NoError(t, json.Unmarshal([]byte(userProfile(t, realm)), &profile))
	for _, a := range profile.Attributes {
		if a["name"] == name {
			return a
		}
	}
	t.Fatalf("the user profile declares no attribute %s", name)
	return nil
}

// importedUser is the realm import's user of that username, or nil.
func importedUser(realm map[string]any, username string) map[string]any {
	for _, u := range realm["users"].([]any) {
		if um := u.(map[string]any); um["username"] == username {
			return um
		}
	}
	return nil
}

// mappersNamed are the protocol mappers of a client named name.
func mappersNamed(c map[string]any, name string) []map[string]any {
	var out []map[string]any
	mappers, _ := c["protocolMappers"].([]any)
	for _, m := range mappers {
		if mm := m.(map[string]any); mm["name"] == name {
			out = append(out, mm)
		}
	}
	return out
}

func strs(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, s := range list {
		out = append(out, fmt.Sprint(s))
	}
	sort.Strings(out)
	return out
}

// Every token chino-api takes (audience chino) comes from one of these
// clients — the ones people watch through, the portal's, and the CLI's, whose
// requests portal-api forwards to chino-api — so each carries the rating cap.
var chinoClients = []string{"chino-mobile", "chino-tv", "chino-web", "zae", "zaentrum-web"}

// The people client: confidential, the client credentials grant and no other
// way in, no redirect, no secret in the import — the realm Job sets the one in
// Secret zaentrum-people — and a service account holding view-users,
// query-users and manage-users of realm-management, and nothing else.
func TestRealmImportHasThePeopleClient(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))
	realm := realmImport(t, objs)
	c := importedClient(t, objs, "zaentrum-people")

	assert.Equal(t, false, c["publicClient"], "a confidential client")
	assert.Equal(t, true, c["serviceAccountsEnabled"], "it signs in as itself")
	for _, flow := range []string{"standardFlowEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled", "bearerOnly"} {
		assert.Equal(t, false, c[flow], "%s: no person signs in through it", flow)
	}
	assert.Equal(t, "false", c["attributes"].(map[string]any)["oauth2.device.authorization.grant.enabled"])
	assert.Empty(t, c["redirectUris"])
	assert.Empty(t, c["webOrigins"])
	assert.NotContains(t, c, "secret", "no secret in a ConfigMap: Keycloak makes one, the realm Job sets the Secret's")
	assert.Empty(t, c["defaultClientScopes"], "its tokens carry no profile, email or roles of the realm")
	assert.Empty(t, c["optionalClientScopes"])

	sa := importedUser(realm, "service-account-zaentrum-people")
	require.NotNil(t, sa, "its service account")
	assert.Equal(t, "zaentrum-people", sa["serviceAccountClientId"])
	assert.NotContains(t, sa, "realmRoles", "no realm role: zaentrum-admin on it would open nothing, and it needs none")
	roles, _ := sa["clientRoles"].(map[string]any)
	require.Len(t, roles, 1, "realm-management roles only")
	assert.Equal(t, []string{"manage-users", "query-users", "view-users"}, strs(roles["realm-management"]),
		"never realm-admin, manage-realm or manage-clients")

	// The people client is no client people sign in through: the realm Job's
	// list of those does not name it, and no redirect is kept for it.
	assert.NotContains(t, jobClientLines(t, realmJob(t, base("zaentrum"))), "zaentrum-people")

	assert.Equal(t, "length(8) and notUsername and notEmail", realm["passwordPolicy"],
		"an invited person chooses a password the realm takes")
}

// A person's rating cap: the user attribute max_rating, an age from 0 to 21,
// declared so that only an admin sees or changes it — a cap the person could
// change would cap nothing — and mapped into the access tokens of every
// client whose tokens chino-api takes as the integer claim max_rating. No
// attribute, no claim: no cap.
func TestRealmImportMapsTheRatingCap(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))
	realm := realmImport(t, objs)

	attr := profileAttribute(t, realm, "max_rating")
	perms := attr["permissions"].(map[string]any)
	assert.Equal(t, []string{"admin"}, strs(perms["view"]), "a person does not see their cap in the account console")
	assert.Equal(t, []string{"admin"}, strs(perms["edit"]), "nor change it")
	assert.NotContains(t, attr, "required", "a person without a cap has no such attribute")
	assert.Equal(t, false, attr["multivalued"])
	integer := attr["validations"].(map[string]any)["integer"].(map[string]any)
	assert.Equal(t, map[string]any{"min": float64(0), "max": float64(21)}, integer, "an age")
	for _, name := range []string{"username", "email", "firstName", "lastName"} {
		profileAttribute(t, realm, name) // Keycloak's own, kept
	}

	for _, c := range realm["clients"].([]any) {
		cm := c.(map[string]any)
		id := cm["clientId"].(string)
		mappers := mappersNamed(cm, "max-rating")
		if !contains(chinoClients, id) {
			assert.Empty(t, mappers, "%s: no token chino-api takes", id)
			continue
		}
		require.Len(t, mappers, 1, id)
		m := mappers[0]
		assert.Equal(t, "oidc-usermodel-attribute-mapper", m["protocolMapper"], id)
		config := m["config"].(map[string]any)
		assert.Equal(t, "max_rating", config["user.attribute"], id)
		assert.Equal(t, "max_rating", config["claim.name"], "%s: the contract with the parental controls", id)
		assert.Equal(t, "int", config["jsonType.label"], "%s: an integer, not a string", id)
		assert.Equal(t, "true", config["access.token.claim"], "%s: chino-api reads the access token", id)
		assert.Equal(t, "false", config["multivalued"], id)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// settingsOf reads a name=value list.
func settingsOf(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range strings.Fields(s) {
		name, value, ok := strings.Cut(kv, "=")
		require.True(t, ok, "a setting is name=value: %q", kv)
		out[name] = value
	}
	return out
}

// The realm Job keeps an existing realm as the import makes a new one: the
// people client — made from the import's own representation, signing in as
// the import says, with the Secret's secret, its service account's roles
// exactly the import's — and the rating cap — the attribute as the import's
// user profile declares it, the mapper on exactly the clients the import
// gives it, with the import's config. One derivation, two readers.
func TestRealmJobKeepsThePeopleClientAndTheRatingCap(t *testing.T) {
	for name, p := range redirectProfiles() {
		objs := renderCR(t, p.cr)
		realm := realmImport(t, objs)
		job := realmJob(t, p.cr)
		env := envByName(job.Spec.Template.Spec.Containers[0])

		assert.Equal(t, "zaentrum-people", env["PEOPLE_CLIENT"].Value, name)
		var rep map[string]any
		require.NoError(t, json.Unmarshal([]byte(env["PEOPLE_CLIENT_JSON"].Value), &rep), name)
		people := importedClient(t, objs, "zaentrum-people")
		assert.Equal(t, people, rep, "%s: the client is made as the import makes it", name)
		want := map[string]string{"oauth2.device.authorization.grant.enabled": "false"}
		for _, f := range []string{"publicClient", "serviceAccountsEnabled", "standardFlowEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled"} {
			want[f] = fmt.Sprint(people[f])
		}
		assert.Equal(t, want, settingsOf(t, env["PEOPLE_SETTINGS"].Value), name)
		sa := importedUser(realm, "service-account-zaentrum-people")
		assert.Equal(t, strs(sa["clientRoles"].(map[string]any)["realm-management"]),
			strings.Fields(env["PEOPLE_ROLES"].Value), "%s: exactly the import's roles", name)
		secretRef(t, env, "PEOPLE_CLIENT_SECRET", "zaentrum-people", "client-secret")

		var rated []string
		var mapper map[string]any
		for _, c := range realm["clients"].([]any) {
			cm := c.(map[string]any)
			if m := mappersNamed(cm, "max-rating"); len(m) == 1 {
				rated = append(rated, cm["clientId"].(string))
				mapper = m[0]
			}
		}
		sort.Strings(rated)
		assert.Equal(t, chinoClients, rated, name)
		assert.Equal(t, rated, strings.Fields(env["RATING_CLIENTS"].Value), name)
		var jobMapper map[string]any
		require.NoError(t, json.Unmarshal([]byte(env["RATING_MAPPER_JSON"].Value), &jobMapper), name)
		assert.Equal(t, mapper, jobMapper, name)
		config := map[string]string{}
		for k, v := range mapper["config"].(map[string]any) {
			config[k] = v.(string)
		}
		assert.Equal(t, config, settingsOf(t, env["RATING_MAPPER_CONFIG"].Value), "%s: the config the Job checks is the mapper's", name)

		var attribute map[string]any
		require.NoError(t, json.Unmarshal([]byte(env["PROFILE_ATTRIBUTE"].Value), &attribute), name)
		assert.Equal(t, profileAttribute(t, realm, "max_rating"), attribute, name)
		// The check is a projection of the attribute, keyed as Keycloak answers
		// one: name, validations, permissions (view, then edit), multivalued.
		check := env["PROFILE_ATTRIBUTE_CHECK"].Value
		var projected map[string]any
		require.NoError(t, json.Unmarshal([]byte(check), &projected), name)
		assert.Equal(t, map[string]any{
			"name": "max_rating", "validations": attribute["validations"], "multivalued": false,
			"permissions": attribute["permissions"],
		}, projected, name)
		assert.True(t, strings.HasPrefix(check, `{"name":"max_rating","validations":{"integer":{"max":21,"min":0}},"permissions":{"view":["admin"],"edit":["admin"]}`),
			"%s: in Keycloak's order: %s", name, check)
		assert.Equal(t, realm["passwordPolicy"], env["PASSWORD_POLICY"].Value, name)
	}
}

// chino-api takes a token for its audience, chino, and caps its person by the
// claim max_rating alone. So the clients whose tokens carry that audience — the
// apps', the portal's, and the CLI's, whose requests portal-api forwards to
// chino-api (an admin deleting someone's data) — are exactly those that carry
// the rating cap, in a new realm as the import makes it and in one that
// exists, as the realm Job keeps it: the same mapper, from the import, on
// exactly those clients, and the cap kept first.
func TestRealmTokensChinoAPITakesCarryTheRatingCap(t *testing.T) {
	for name, p := range redirectProfiles() {
		objs := renderCR(t, p.cr)
		realm := realmImport(t, objs)

		var audienced []string
		var mapper map[string]any
		for _, c := range realm["clients"].([]any) {
			cm := c.(map[string]any)
			id := cm["clientId"].(string)
			m := mappersNamed(cm, "audience-chino")
			if len(m) == 0 {
				continue
			}
			require.Len(t, m, 1, "%s: %s", name, id)
			audienced = append(audienced, id)
			if mapper != nil {
				assert.Equal(t, mapper, m[0], "%s: %s's audience mapper is everyone's", name, id)
			}
			mapper = m[0]
			assert.Len(t, mappersNamed(cm, "max-rating"), 1, "%s: %s's tokens are chino-api's, and carry no cap", name, id)
		}
		sort.Strings(audienced)
		assert.Equal(t, chinoClients, audienced, "%s: the clients whose tokens chino-api takes", name)
		require.NotNil(t, mapper, name)
		assert.Equal(t, "oidc-audience-mapper", mapper["protocolMapper"], name)
		config := map[string]string{}
		for k, v := range mapper["config"].(map[string]any) {
			config[k] = v.(string)
		}
		assert.Equal(t, "chino", config["included.custom.audience"], "%s: chino-api's OIDC_AUDIENCE", name)
		assert.Equal(t, "true", config["access.token.claim"], "%s: chino-api reads the access token", name)
		assert.Equal(t, "false", config["id.token.claim"], name)

		// The realm Job keeps an existing realm so, from the same import.
		env := envByName(realmJob(t, p.cr).Spec.Template.Spec.Containers[0])
		assert.Equal(t, audienced, strings.Fields(env["AUDIENCE_CLIENTS"].Value), name)
		assert.Equal(t, strings.Fields(env["RATING_CLIENTS"].Value), strings.Fields(env["AUDIENCE_CLIENTS"].Value),
			"%s: the Job caps every client it gives the audience", name)
		var jobMapper map[string]any
		require.NoError(t, json.Unmarshal([]byte(env["AUDIENCE_MAPPER_JSON"].Value), &jobMapper), name)
		assert.Equal(t, mapper, jobMapper, "%s: the mapper is made as the import makes it", name)
		assert.Equal(t, config, settingsOf(t, env["AUDIENCE_MAPPER_CONFIG"].Value), "%s: the config the Job checks is the mapper's", name)
	}

	// The cap first, then the audience: a run that stops on the way never
	// leaves a client with the audience and without the cap.
	script, err := os.ReadFile("../../platform/chart/files/realm-config.sh")
	require.NoError(t, err)
	capped := strings.Index(string(script), `keep_mapper "$RATING_MAPPER_JSON"`)
	audience := strings.Index(string(script), `keep_mapper "$AUDIENCE_MAPPER_JSON"`)
	require.Positive(t, capped, "the realm Job keeps the rating cap")
	require.Positive(t, audience, "the realm Job keeps the audience")
	assert.Less(t, capped, audience, "the rating cap is kept before the audience")
}

// container is a Deployment's first container, typed.
func container(t *testing.T, objs []*unstructured.Unstructured, name string) corev1.Container {
	t.Helper()
	u := find(t, objs, "Deployment", name)
	require.NotNil(t, u, "Deployment %s", name)
	var d appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &d))
	require.NotEmpty(t, d.Spec.Template.Spec.Containers)
	return d.Spec.Template.Spec.Containers[0]
}

// Bundled, portal-api manages people through the in-cluster Keycloak with the
// people client, its secret from Secret zaentrum-people — optional, so that a
// platform whose Secrets someone else makes starts without it, and the page
// says it is not set up — and portal-api and chino-api share the account
// deletion token. With an external provider none of it is there, and nothing
// refers to Secret zaentrum-people.
func TestPeopleAreManagedOnlyWithBundledIdentity(t *testing.T) {
	bundled := map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"helm install": helmRender(t, nil),
	}
	for name, objs := range bundled {
		portal := envByName(container(t, objs, "portal-api"))
		assert.Equal(t, "http://keycloak:80/auth", portal["PORTAL_PEOPLE_KEYCLOAK_URL"].Value, "%s: the in-cluster Keycloak", name)
		assert.Equal(t, "zaentrum", portal["PORTAL_PEOPLE_REALM"].Value, name)
		assert.Equal(t, "zaentrum-people", portal["PORTAL_PEOPLE_CLIENT_ID"].Value, name)
		secretRef(t, portal, "PORTAL_PEOPLE_CLIENT_SECRET", "zaentrum-people", "client-secret")
		secretRef(t, portal, "PORTAL_ACCOUNT_DELETION_TOKEN", "zaentrum-people", "deletion-token")
		assert.Equal(t, "http://chino-api", portal["PORTAL_CHINO_API_URL"].Value, name)
		assert.Equal(t, realmImport(t, objs)["passwordPolicy"], portal["PORTAL_PASSWORD_POLICY"].Value, name)

		chino := envByName(container(t, objs, "chino-api"))
		secretRef(t, chino, "ACCOUNT_DELETION_TOKEN", "zaentrum-people", "deletion-token")
	}

	ext := base("zaentrum-beta")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	ext.Spec.Identity.ClientID = "media-web"
	external := map[string][]*unstructured.Unstructured{
		"operator": renderCR(t, ext),
		"helm install": helmRender(t, map[string]interface{}{"identity": map[string]interface{}{
			"mode": "external", "issuer": "https://sso.example.org/realms/example",
		}}),
	}
	for name, objs := range external {
		for _, d := range []string{"portal-api", "chino-api"} {
			for _, e := range container(t, objs, d).Env {
				assert.False(t, strings.HasPrefix(e.Name, "PORTAL_PEOPLE_") || strings.Contains(e.Name, "DELETION") ||
					e.Name == "PORTAL_PASSWORD_POLICY" || e.Name == "PORTAL_CHINO_API_URL", "%s: %s sets %s", name, d, e.Name)
			}
		}
		assert.NotContains(t, fmt.Sprintf("%v", objs), "zaentrum-people", "%s: no reference to the bundled realm's people", name)
	}
}
