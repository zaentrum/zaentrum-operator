package templates

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// realmClient is the part of a realm client this test reads.
type realmClient struct {
	RedirectURIs []string          `json:"redirectUris"`
	WebOrigins   []string          `json:"webOrigins"`
	Attributes   map[string]string `json:"attributes"`
}

func (c realmClient) postLogout() []string {
	v := c.Attributes["post.logout.redirect.uris"]
	if v == "" {
		return nil
	}
	return strings.Split(v, "##")
}

// realmImport is the realm JSON the render's keycloak-realm ConfigMap carries.
func realmImport(t *testing.T, objs []*unstructured.Unstructured) map[string]any {
	t.Helper()
	cm := find(t, objs, "ConfigMap", "keycloak-realm")
	require.NotNil(t, cm, "no realm import")
	raw, _, _ := unstructured.NestedString(cm.Object, "data", "zaentrum-realm.json")
	var realm map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &realm), "the realm import is JSON")
	return realm
}

// realmClients is the import's clients by clientId.
func realmClients(t *testing.T, objs []*unstructured.Unstructured) map[string]realmClient {
	t.Helper()
	realm := realmImport(t, objs)
	out := map[string]realmClient{}
	for _, c := range realm["clients"].([]any) {
		b, _ := json.Marshal(c)
		var rc realmClient
		require.NoError(t, json.Unmarshal(b, &rc))
		out[c.(map[string]any)["clientId"].(string)] = rc
	}
	return out
}

func renderCR(t *testing.T, z *zaentrumv1alpha1.Zaentrum) []*unstructured.Unstructured {
	t.Helper()
	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	return objs
}

// redirectProfiles are the ways the platform is reached, and the origins it
// answers on in each.
func redirectProfiles() map[string]struct {
	cr       *zaentrumv1alpha1.Zaentrum
	main     []string
	chino    []string
	verifyAt string
} {
	yes := true
	tls := base("zaentrum")
	tls.Spec.Hostname = "media.example.org"
	tls.Spec.Identity.IssuerScheme = "https"

	sub := base("zaentrum")
	sub.Spec.Hostname = "media.example.org"
	sub.Spec.Routing.ProvisionRoutes = &yes
	sub.Spec.Routing.Mode = "subdomains"
	sub.Spec.Routing.Hosts.Chino = "chino.example.org"

	return map[string]struct {
		cr       *zaentrumv1alpha1.Zaentrum
		main     []string
		chino    []string
		verifyAt string
	}{
		// The appliance: a plain Ingress, http.
		"self-host": {base("zaentrum"), []string{"http://zaentrum.localhost"}, nil, "http://zaentrum.localhost"},
		// TLS terminated by a proxy in front of the Ingress.
		"https proxy": {tls, []string{"https://media.example.org"}, nil, "https://media.example.org"},
		// OpenShift Routes terminate TLS and allow plain http beside it.
		"demo": {demoCR("zaentrum-demo"), []string{"https://zaentrum.demo.nalet.cloud", "http://zaentrum.demo.nalet.cloud"}, nil, // neutrality-guard:allow
			"https://zaentrum.demo.nalet.cloud"}, // neutrality-guard:allow
		// chino on a host of its own, https only (its Routes redirect http).
		"subdomains": {sub, []string{"https://media.example.org", "http://media.example.org"}, []string{"https://chino.example.org"},
			"https://media.example.org"},
	}
}

func suffixed(origins []string, suffixes ...string) []string {
	var out []string
	for _, o := range origins {
		for _, s := range suffixes {
			out = append(out, o+s)
		}
	}
	return out
}

// The clients people sign in through return only to the platform's own
// origins and paths — what each client builds — and nowhere else.
func TestRealmClientsAllowOnlyThePlatformsOwnRedirects(t *testing.T) {
	for name, p := range redirectProfiles() {
		t.Run(name, func(t *testing.T) {
			clients := realmClients(t, renderCR(t, p.cr))
			web := append(append([]string{}, p.main...), p.chino...)

			chino := clients["chino-web"]
			assert.Equal(t, suffixed(web, "/auth/callback"), chino.RedirectURIs,
				"chino-web returns to the site root's /auth/callback, whatever its base")
			assert.Equal(t, web, chino.WebOrigins)
			assert.Equal(t, web, chino.postLogout(), "signed out, chino-web returns to the bare origin")

			portal := clients["zaentrum-web"]
			assert.Equal(t, suffixed(p.main, "/portal/auth/callback", "/katalog/auth/callback", "/katalog-manage/auth/callback"),
				portal.RedirectURIs, "the portal and the two katalog consoles sign in as zaentrum-web")
			assert.Equal(t, p.main, portal.WebOrigins)
			assert.Equal(t, suffixed(p.main, "/portal/", "/katalog/", "/katalog-manage/"), portal.postLogout())

			mobile := clients["chino-mobile"]
			assert.Equal(t, []string{"cloud.nalet.chino:/oauth/callback"}, mobile.RedirectURIs, "the published apps' scheme")
			assert.Empty(t, mobile.WebOrigins)
			assert.Equal(t, "+", mobile.Attributes["post.logout.redirect.uris"], "the redirect above, nothing else")

			assert.Empty(t, clients["chino-tv"].RedirectURIs, "the device grant redirects nowhere")
			assert.Empty(t, clients["chino-tv"].WebOrigins)
			assert.Equal(t, []string{"http://127.0.0.1/*", "http://localhost/*"}, clients["zae"].RedirectURIs, "the CLI's loopback")
			assert.Empty(t, clients["zae"].WebOrigins)
			assert.Empty(t, clients["zaentrum-manager"].RedirectURIs, "a service account signs in through no browser")

			for id, c := range clients {
				for _, list := range [][]string{c.RedirectURIs, c.WebOrigins, c.postLogout()} {
					for _, u := range list {
						assert.NotEqual(t, "*", u, "%s allows any redirect", id)
						if id != "zae" && u != "+" {
							assert.NotContains(t, u, "*", "%s: a wildcard in %q", id, u)
						}
					}
				}
				assert.NotContains(t, c.WebOrigins, "+", "%s: web origins are listed, not derived", id)
			}

			// The platform's own check signs in as chino-web, back to its --url's
			// /auth/callback: that must be among them.
			_, hooks := SplitHooks(renderCR(t, p.cr))
			job := VerifyJob(hooks)
			require.NotNil(t, job)
			containers, _, _ := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
			args := containers[0].(map[string]any)["args"].([]any)
			require.Equal(t, "--url", args[1])
			assert.Equal(t, p.verifyAt, args[2])
			assert.Contains(t, chino.RedirectURIs, p.verifyAt+"/auth/callback")
		})
	}
}

// Everything else in the realm import is the file's, as it was.
func TestRealmImportKeepsTheRestOfTheRealm(t *testing.T) {
	raw, err := os.ReadFile("../../platform/chart/files/keycloak-realm.json")
	require.NoError(t, err)
	var file map[string]any
	require.NoError(t, json.Unmarshal(raw, &file))

	managed := map[string]bool{"zaentrum-web": true, "chino-web": true, "chino-mobile": true, "chino-tv": true, "zae": true}
	strip := func(realm map[string]any) map[string]any {
		for _, c := range realm["clients"].([]any) {
			cm := c.(map[string]any)
			if !managed[cm["clientId"].(string)] {
				continue
			}
			delete(cm, "redirectUris")
			delete(cm, "webOrigins")
			if attrs, ok := cm["attributes"].(map[string]any); ok {
				delete(attrs, "post.logout.redirect.uris")
			}
		}
		return realm
	}
	for name, p := range redirectProfiles() {
		got := strip(realmImport(t, renderCR(t, p.cr)))
		var want map[string]any
		require.NoError(t, json.Unmarshal(raw, &want))
		assert.Equal(t, strip(want), got, name)
	}
	for _, c := range file["clients"].([]any) {
		cm := c.(map[string]any)
		if managed[cm["clientId"].(string)] {
			assert.Empty(t, cm["redirectUris"], "%s: the file leaves the managed redirects empty; z.realmClients fills them", cm["clientId"])
			assert.Empty(t, cm["webOrigins"], cm["clientId"])
		}
	}
}

// route is one path the platform's host serves and the Service behind it.
type route struct{ host, path, service string }

// routesOf reads every Ingress path and OpenShift Route the render publishes.
func routesOf(t *testing.T, objs []*unstructured.Unstructured) []route {
	t.Helper()
	var out []route
	for _, o := range objs {
		switch o.GetKind() {
		case "Ingress":
			rules, _, _ := unstructured.NestedSlice(o.Object, "spec", "rules")
			for _, r := range rules {
				rm := r.(map[string]any)
				paths, _, _ := unstructured.NestedSlice(rm, "http", "paths")
				for _, p := range paths {
					pm := p.(map[string]any)
					svc, _, _ := unstructured.NestedString(pm, "backend", "service", "name")
					out = append(out, route{rm["host"].(string), pm["path"].(string), svc})
				}
			}
		case "Route":
			host, _, _ := unstructured.NestedString(o.Object, "spec", "host")
			path, _, _ := unstructured.NestedString(o.Object, "spec", "path")
			svc, _, _ := unstructured.NestedString(o.Object, "spec", "to", "name")
			out = append(out, route{host, path, svc})
		}
	}
	return out
}

// serving is the Service a request for host+path reaches: the longest path
// prefix on that host, as the Ingress controllers and the OpenShift router
// match.
func serving(routes []route, host, path string) string {
	best, svc := -1, ""
	for _, r := range routes {
		if r.host != host || !strings.HasPrefix(path, r.path) || len(r.path) <= best {
			continue
		}
		// A prefix matches on a path-segment boundary.
		if len(path) > len(r.path) && !strings.HasSuffix(r.path, "/") && path[len(r.path)] != '/' {
			continue
		}
		best, svc = len(r.path), r.service
	}
	return svc
}

// Every redirect a client may return to is served by that client's SPA:
// a registered callback that reached Keycloak or the portal would end the
// sign-in on an error page.
func TestEveryRedirectReachesItsClient(t *testing.T) {
	want := map[string]string{
		"/auth/callback":                "chino-web",
		"/portal/auth/callback":         "zaentrum-portal",
		"/katalog/auth/callback":        "katalog-manager-ui",
		"/katalog-manage/auth/callback": "katalog-manage-ui",
	}
	for name, p := range redirectProfiles() {
		objs := renderCR(t, p.cr)
		routes := routesOf(t, objs)
		clients := realmClients(t, objs)
		var checked []string
		for _, id := range []string{"chino-web", "zaentrum-web"} {
			for _, u := range clients[id].RedirectURIs {
				rest := u[strings.Index(u, "://")+3:]
				host, path := rest[:strings.Index(rest, "/")], rest[strings.Index(rest, "/"):]
				assert.Equal(t, want[path], serving(routes, host, path), "%s: %s", name, u)
				checked = append(checked, u)
			}
		}
		sort.Strings(checked)
		assert.NotEmpty(t, checked, name)
	}
}
