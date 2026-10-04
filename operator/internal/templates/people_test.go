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
// service account may view, query and manage users and do nothing else.
// Bundled identity only: an external provider's people are its own.

// importedUser is the realm import's user of that username, or nil.
func importedUser(realm map[string]any, username string) map[string]any {
	for _, u := range realm["users"].([]any) {
		if um := u.(map[string]any); um["username"] == username {
			return um
		}
	}
	return nil
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

// The realm Job keeps an existing realm's people client as the import makes
// a new one: made from the import's own representation, signing in as the
// import says, with the Secret's secret, its service account's roles exactly
// the import's. One derivation, two readers.
func TestRealmJobKeepsThePeopleClient(t *testing.T) {
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
		assert.Equal(t, realm["passwordPolicy"], env["PASSWORD_POLICY"].Value, name)
	}
}
