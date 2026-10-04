package templates

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The viewers watch through these clients: every token chino-api takes
// (audience chino) comes from one of them, so each carries the rating cap.
var viewerClients = []string{"chino-mobile", "chino-tv", "chino-web", "zaentrum-web"}

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
// client people watch through as the integer claim max_rating. No attribute,
// no claim: no cap.
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
		if !contains(viewerClients, id) {
			assert.Empty(t, mappers, "%s: no viewer's token", id)
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
		assert.Equal(t, viewerClients, rated, name)
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
