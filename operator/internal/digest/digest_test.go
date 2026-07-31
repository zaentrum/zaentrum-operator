package digest

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in       string
		reg      string
		repo     string
		tag      string
		digest   string
		isPinned bool
	}{
		{"ghcr.io/zaentrum/portal-api:latest", "ghcr.io", "zaentrum/portal-api", "latest", "", false},
		{"ghcr.io/zaentrum/portal-api:v0.4.0", "ghcr.io", "zaentrum/portal-api", "v0.4.0", "", false},
		{"ghcr.io/zaentrum/acquire@sha256:abcd", "ghcr.io", "zaentrum/acquire", "", "sha256:abcd", true},
		{"postgres:16-alpine", "docker.io", "postgres", "16-alpine", "", false},
		{"valkey/valkey:8-alpine", "docker.io", "valkey/valkey", "8-alpine", "", false},
		{"quay.io/keycloak/keycloak:26.0.7", "quay.io", "keycloak/keycloak", "26.0.7", "", false},
		{"localhost:5000/x/y:1", "localhost:5000", "x/y", "1", "", false},
	} {
		got := Parse(tc.in)
		if got.Registry != tc.reg || got.Repo != tc.repo || got.Tag != tc.tag || got.Digest != tc.digest {
			t.Errorf("Parse(%q) = %+v", tc.in, got)
		}
		if got.Pinned() != tc.isPinned {
			t.Errorf("Parse(%q).Pinned() = %v", tc.in, got.Pinned())
		}
	}
}

// Only ghcr.io/zaentrum/* images may be pinned; third-party stays put.
func TestZaentrumImagesMatch(t *testing.T) {
	yes := []string{"ghcr.io/zaentrum/portal-api:latest", "ghcr.io/zaentrum/acquire:latest"}
	no := []string{"postgres:16-alpine", "valkey/valkey:8-alpine", "quay.io/keycloak/keycloak:26.0.7", "ghcr.io/other/thing:latest"}
	for _, i := range yes {
		if !ZaentrumImages(Parse(i)) {
			t.Errorf("%q should match", i)
		}
	}
	for _, i := range no {
		if ZaentrumImages(Parse(i)) {
			t.Errorf("%q should NOT match", i)
		}
	}
}

func TestCredsFromDockerConfig(t *testing.T) {
	body := []byte(`{"auths":{"ghcr.io":{"auth":"` +
		base64Std("bob:ghp_token") + `"},"quay.io":{"username":"u","password":"p"}}}`)
	creds, err := CredsFromDockerConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	if creds["ghcr.io"] != "bob:ghp_token" || creds["quay.io"] != "u:p" {
		t.Fatalf("creds = %v", creds)
	}
}

// A registry that challenges for a token, then serves the manifest digest, must
// be resolved to registry/repo@digest — the whole point of the feature.
func TestResolveAgainstFakeRegistry(t *testing.T) {
	const wantDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "abc"})
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", wantDigest)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	rv := New(nil)
	rv.Scheme = "http" // talk to the test server over http
	rv.HTTP = srv.Client()

	image := host + "/zaentrum/portal-api:latest"
	got, err := rv.Resolve(context.Background(), image)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := host + "/zaentrum/portal-api@" + wantDigest
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	// Second call is served from cache.
	if again, _ := rv.Resolve(context.Background(), image); again != want {
		t.Fatalf("cached resolve = %q", again)
	}
}

// PinImages rewrites matching container images and leaves the rest alone.
func TestPinImagesRewritesOnlyZaentrum(t *testing.T) {
	dep := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{
				map[string]any{"name": "seed", "image": "postgres:16-alpine"},
			},
			"containers": []any{
				map[string]any{"name": "app", "image": "ghcr.io/zaentrum/portal-api:latest"},
				map[string]any{"name": "cache", "image": "valkey/valkey:8-alpine"},
			},
		}}},
	}}
	rv := New(nil)
	// A resolver stub: pretend every zaentrum image pins to a fixed digest.
	rv.cache["ghcr.io/zaentrum/portal-api:latest"] = cacheEntry{
		ref: "ghcr.io/zaentrum/portal-api@sha256:deadbeef", at: time.Now(),
	}
	n, errs := rv.PinImages(context.Background(), []*unstructured.Unstructured{dep}, ZaentrumImages)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if n != 1 {
		t.Fatalf("pinned %d, want 1", n)
	}
	containers, _ := podContainers(dep)
	got := containers[0][0].(map[string]any)["image"]
	if got != "ghcr.io/zaentrum/portal-api@sha256:deadbeef" {
		t.Errorf("app image = %v", got)
	}
	// postgres + valkey untouched
	if containers[1][0].(map[string]any)["image"] != "postgres:16-alpine" {
		t.Error("postgres was rewritten")
	}
}

func base64Std(s string) string { return b64.StdEncoding.EncodeToString([]byte(s)) }

func TestValidDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	if !validDigest(good) {
		t.Error("well-formed digest rejected")
	}
	for _, bad := range []string{"", "sha256:", "sha256:abcd", "sha512:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("a", 63)} {
		if validDigest(bad) {
			t.Errorf("malformed digest accepted: %q", bad)
		}
	}
}

// A malformed digest header must NOT be pinned — better keep the tag than apply
// an unpullable image.
func TestResolveRejectsMalformedDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef") // truncated
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rv := New(nil)
	rv.Scheme = "http"
	rv.HTTP = srv.Client()
	if _, err := rv.Resolve(context.Background(), strings.TrimPrefix(srv.URL, "http://")+"/zaentrum/x:latest"); err == nil {
		t.Fatal("a truncated digest should be rejected")
	}
}

// A transient registry failure after a successful pin must serve the last-known
// digest, not revert to the tag (which would flap the deployment under SSA).
func TestResolveServesLastKnownOnFailure(t *testing.T) {
	const dgst = "sha256:" + "1234567890123456789012345678901234567890123456789012345678901234"
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rv := New(nil)
	rv.Scheme = "http"
	rv.HTTP = srv.Client()
	rv.TTL = 0 // force a fresh fetch every call so the failure path is exercised
	image := strings.TrimPrefix(srv.URL, "http://") + "/zaentrum/x:latest"
	want := strings.TrimPrefix(srv.URL, "http://") + "/zaentrum/x@" + dgst

	got, err := rv.Resolve(context.Background(), image)
	if err != nil || got != want {
		t.Fatalf("initial resolve = %q, %v", got, err)
	}
	fail = true // registry now down
	got2, err := rv.Resolve(context.Background(), image)
	if err != nil {
		t.Fatalf("should have served last-known digest, got err: %v", err)
	}
	if got2 != want {
		t.Fatalf("served %q on failure, want last-known %q", got2, want)
	}
}

// The pull-secret credential must never be sent to a token host the registry
// challenge points elsewhere — only to the same host we pull from.
func TestTokenCredentialStaysOnRegistryHost(t *testing.T) {
	var evilGotAuth, ownGotAuth bool
	var own *httptest.Server
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			evilGotAuth = true
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "x"})
	}))
	defer evil.Close()
	own = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			if r.Header.Get("Authorization") != "" {
				ownGotAuth = true
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "x"})
		default:
			// Challenge points the token endpoint at the EVIL host.
			w.Header().Set("Www-Authenticate", `Bearer realm="`+evil.URL+`/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer own.Close()

	ownHost := strings.TrimPrefix(own.URL, "http://")
	rv := New(map[string]string{ownHost: "bob:ghp_secret"})
	rv.Scheme = "http"
	rv.HTTP = own.Client()
	// A cross-host realm can't use TLS-matched creds anyway; the point is the
	// PAT must not leak. (Resolve will fail because evil returns no digest.)
	_, _ = rv.Resolve(context.Background(), ownHost+"/zaentrum/x:latest")
	if evilGotAuth {
		t.Fatal("credential was sent to the challenge-supplied (foreign) token host")
	}
	_ = ownGotAuth
}
