// Package digest resolves a moving image tag (ghcr.io/zaentrum/portal-api:latest)
// to the immutable content digest it currently points at
// (ghcr.io/zaentrum/portal-api@sha256:…).
//
// This is what makes "push a new image, the operator rolls it" true. The
// operator renders the platform from a Helm chart and applies it with
// server-side apply; when spec.version is a moving tag, a fresh push leaves the
// rendered spec byte-identical, so SSA is a no-op and nothing restarts. Pinning
// each image to its current digest at render time means a new push changes the
// rendered spec, SSA sees a real diff, and Kubernetes rolls exactly the
// components whose bytes changed.
//
// Resolution is best-effort by design: the caller falls back to the tag on any
// failure, so a slow or unreachable registry can never block a reconcile.
package digest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// acceptManifest lists every manifest media type a digest lookup may meet: a
// multi-arch index or a single-arch manifest, OCI or Docker. The registry
// returns the digest of whichever it served in Docker-Content-Digest.
var acceptManifest = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Resolver maps image references to digests, with a short cache so a frequent
// reconcile loop does not hammer the registry for a tag that rarely moves.
type Resolver struct {
	HTTP *http.Client
	TTL  time.Duration
	// Scheme is the registry scheme, "https" in production; tests point it at a
	// local server over "http".
	Scheme string

	// creds holds registry host -> "user:token" basic credentials, harvested
	// from a pull secret. A host with no entry is queried anonymously.
	creds map[string]string

	mu    sync.Mutex
	cache map[string]cacheEntry // key: full ref with tag -> digest ref
}

type cacheEntry struct {
	ref string
	at  time.Time
}

// New returns a resolver. creds may be nil (all lookups anonymous).
func New(creds map[string]string) *Resolver {
	return &Resolver{
		HTTP:   &http.Client{Timeout: 10 * time.Second},
		TTL:    60 * time.Second,
		Scheme: "https",
		creds:  creds,
		cache:  map[string]cacheEntry{},
	}
}

// SetCreds swaps the registry credentials (a reconcile may see a refreshed pull
// secret). Safe to call between resolves.
func (rv *Resolver) SetCreds(creds map[string]string) {
	rv.mu.Lock()
	rv.creds = creds
	rv.mu.Unlock()
}

// CredsFromDockerConfig extracts registry -> "user:token" from a
// .dockerconfigjson body, so a pull secret can authenticate digest lookups for
// private repositories.
func CredsFromDockerConfig(dockerconfigjson []byte) (map[string]string, error) {
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(dockerconfigjson, &cfg); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for host, a := range cfg.Auths {
		switch {
		case a.Auth != "":
			if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				out[normalizeHost(host)] = string(dec)
			}
		case a.Username != "":
			out[normalizeHost(host)] = a.Username + ":" + a.Password
		}
	}
	return out, nil
}

// Ref is the parsed pieces of an image reference.
type Ref struct {
	Registry string // ghcr.io
	Repo     string // zaentrum/portal-api
	Tag      string // latest
	Digest   string // sha256:… when already pinned
}

// Pinned reports whether the reference already carries a digest.
func (r Ref) Pinned() bool { return r.Digest != "" }

// Parse splits an image reference into registry, repository, tag and/or digest.
// A reference with no registry (a bare docker.io short name) yields Registry
// "docker.io", which the zaentrum-only caller simply skips.
func Parse(image string) Ref {
	var ref Ref
	rest := image
	if i := strings.Index(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
	}
	// The registry is the first slash-segment IF it looks like a host
	// (contains a dot or a colon, or is localhost); otherwise it is docker.io.
	slash := strings.Index(rest, "/")
	if slash >= 0 {
		first := rest[:slash]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			ref.Registry = first
			rest = rest[slash+1:]
		} else {
			ref.Registry = "docker.io"
		}
	} else {
		ref.Registry = "docker.io"
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		ref.Tag = rest[i+1:]
		rest = rest[:i]
	}
	ref.Repo = rest
	return ref
}

// Resolve returns the image reference with its tag replaced by the digest it
// currently points at. An already-pinned reference is returned unchanged.
func (rv *Resolver) Resolve(ctx context.Context, image string) (string, error) {
	ref := Parse(image)
	if ref.Pinned() {
		return image, nil
	}
	if ref.Tag == "" {
		ref.Tag = "latest"
	}

	if fresh, ok := rv.cached(image, false); ok {
		return fresh, nil
	}

	dgst, err := rv.fetchDigest(ctx, ref)
	if err != nil {
		// Ride out a transient registry failure on the LAST-KNOWN digest rather
		// than reverting to the tag. Reverting would flip the applied image from
		// @sha256:… back to :latest, and because the operator force-owns the
		// image field, server-side apply would then roll the component back to
		// the tag on a mere registry blip — an unwanted restart. Only a
		// successful fetch (a real push) changes the pin.
		if stale, ok := rv.cached(image, true); ok {
			return stale, nil
		}
		return "", err
	}
	pinned := ref.Registry + "/" + ref.Repo + "@" + dgst
	rv.store(image, pinned)
	return pinned, nil
}

// cached returns a cached pin. allowStale ignores the TTL, so a resolve that
// cannot reach the registry can still return the previously-resolved digest.
func (rv *Resolver) cached(key string, allowStale bool) (string, bool) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	e, ok := rv.cache[key]
	if !ok {
		return "", false
	}
	if !allowStale && time.Since(e.at) > rv.TTL {
		return "", false
	}
	return e.ref, true
}

func (rv *Resolver) store(key, ref string) {
	rv.mu.Lock()
	rv.cache[key] = cacheEntry{ref: ref, at: time.Now()}
	rv.mu.Unlock()
}

// fetchDigest asks the registry for the manifest digest of ref's tag. It does a
// token exchange first when the registry challenges for one (ghcr always does).
func (rv *Resolver) fetchDigest(ctx context.Context, ref Ref) (string, error) {
	base := rv.scheme() + "://" + ref.Registry + "/v2/" + ref.Repo + "/manifests/" + ref.Tag

	do := func(bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, base, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", acceptManifest)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return rv.HTTP.Do(req)
	}

	resp, err := do("")
	if err != nil {
		return "", err
	}
	// A challenge means we need a token; mint one and retry once.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		token, terr := rv.token(ctx, ref, challenge)
		if terr != nil {
			return "", terr
		}
		resp, err = do(token)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest %s: %s", ref.Repo+":"+ref.Tag, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !validDigest(d) {
		// The header is the only proof of identity for a HEAD (no body to hash),
		// so a malformed value must be refused — pinning to it would apply an
		// unpullable image and wedge the component in ImagePullBackOff.
		return "", fmt.Errorf("manifest %s: malformed digest %q", ref.Repo+":"+ref.Tag, d)
	}
	return d, nil
}

// validDigest checks a registry digest is a well-formed sha256 (algorithm plus
// exactly 64 lowercase hex chars).
func validDigest(d string) bool {
	const p = "sha256:"
	if !strings.HasPrefix(d, p) {
		return false
	}
	hex := d[len(p):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// token performs the registry token exchange from a Www-Authenticate Bearer
// challenge, adding basic auth from the pull secret when we hold credentials for
// the registry (needed for private repositories).
func (rv *Resolver) token(ctx context.Context, ref Ref, challenge string) (string, error) {
	realm, service, scope := parseBearerChallenge(challenge)
	if realm == "" {
		// Sensible default for ghcr / docker-style registries.
		realm = rv.scheme() + "://" + ref.Registry + "/token"
		service = ref.Registry
	}
	if scope == "" {
		scope = "repository:" + ref.Repo + ":pull"
	}

	// The realm comes from the registry's own challenge, so it is untrusted:
	// build the URL with net/url (never string concatenation — a scope or realm
	// with reserved characters would otherwise corrupt the query), and only ever
	// attach our credential when the token host is the SAME host we are pulling
	// from, over TLS. A hostile or MITM'd challenge naming realm="https://evil/…"
	// then gets an anonymous token attempt, never the pull-secret PAT.
	realmURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("token realm %q: %w", realm, err)
	}
	q := realmURL.Query()
	q.Set("service", service)
	q.Set("scope", scope)
	realmURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realmURL.String(), nil)
	if err != nil {
		return "", err
	}
	sameHost := realmURL.Scheme == "https" && normalizeHost(realmURL.Host) == normalizeHost(ref.Registry)
	if sameHost {
		rv.mu.Lock()
		cred := rv.creds[normalizeHost(ref.Registry)]
		rv.mu.Unlock()
		if cred != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cred)))
		}
	}
	resp, err := rv.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token for %s: %s", ref.Repo, resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseBearerChallenge pulls realm/service/scope out of a
// `Bearer realm="…",service="…",scope="…"` header.
func parseBearerChallenge(h string) (realm, service, scope string) {
	h = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		v := strings.Trim(kv[1], `"`)
		switch kv[0] {
		case "realm":
			realm = v
		case "service":
			service = v
		case "scope":
			scope = v
		}
	}
	return
}

// scheme returns the registry scheme, defaulting to https.
func (rv *Resolver) scheme() string {
	if rv.Scheme == "" {
		return "https"
	}
	return rv.Scheme
}

// normalizeHost maps docker's canonical index host to the value that appears in
// image references, so a pull secret keyed either way still matches.
func normalizeHost(h string) string {
	switch h {
	case "https://index.docker.io/v1/", "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return h
}
