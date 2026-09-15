package addon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/remotes/docker"
	"helm.sh/helm/v3/pkg/registry"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

const (
	// MaxArchiveSize bounds a chart archive, and every registry response on
	// the way to one.
	MaxArchiveSize = 10 << 20
	// FetchTimeout bounds one chart fetch end to end.
	FetchTimeout = 30 * time.Second
	// DefaultRefTTL is how long an unpinned chart ref is served from the cache
	// before it is fetched again.
	DefaultRefTTL = 5 * time.Minute

	// maxCachedBytes bounds the archive cache; the oldest archives go first.
	maxCachedBytes = 32 << 20
)

// Archive is a fetched chart archive.
type Archive struct {
	Data []byte
	// Digest is the sha256 of Data ("sha256:<hex>").
	Digest string
}

// Fetcher fetches chart archives: OCI refs through the Helm registry client,
// anonymously, and direct https links over plain HTTP GET. Archives are cached
// by digest, so a pinned chart is fetched once per operator process.
//
// The zero value is usable.
type Fetcher struct {
	// Transport is the HTTP transport; nil uses http.DefaultTransport.
	Transport http.RoundTripper
	// PlainHTTP talks to OCI registries over http. Tests only.
	PlainHTTP bool
	// TTL overrides DefaultRefTTL.
	TTL time.Duration

	mu       sync.Mutex
	archives map[string]*cachedArchive // by archive digest, and by OCI manifest digest
	refs     map[string]refEntry       // by ref+version: the digest last fetched for it
}

type cachedArchive struct {
	archive *Archive
	at      time.Time
}

type refEntry struct {
	digest string
	at     time.Time
}

// Fetch returns the archive for a chart reference.
//
// A pinned digest already in the cache is served without touching the
// network. An unpinned ref is served from the cache until its TTL expires, or
// fetched right away when fresh is set (the addon's spec changed). When a
// fetch of an unpinned ref fails, the archive last fetched for it is served
// instead: a registry blip must not fail an addon that is already installed.
func (f *Fetcher) Fetch(ctx context.Context, c zaentrumv1alpha1.AddonChart, fresh bool) (*Archive, error) {
	if err := ValidateChart(c); err != nil {
		return nil, err
	}
	key := refKey(c)
	if c.Digest != "" {
		if a := f.cachedDigest(c.Digest); a != nil {
			return a, nil
		}
	} else if !fresh {
		if a := f.cachedRef(key, false); a != nil {
			return a, nil
		}
	}

	data, aliases, err := f.fetch(ctx, c)
	if err != nil {
		if c.Digest == "" {
			if a := f.cachedRef(key, true); a != nil {
				return a, nil
			}
		}
		return nil, err
	}
	a := &Archive{Data: data, Digest: Digest(data)}
	if c.Digest != "" && c.Digest != a.Digest && !contains(aliases, c.Digest) {
		return nil, fmt.Errorf("chart digest mismatch: the fetched archive is %s, chart.digest pins %s", a.Digest, c.Digest)
	}
	f.store(key, a, aliases)
	return a, nil
}

// refKey identifies what an unpinned fetch resolves: the version only matters
// for OCI refs.
func refKey(c zaentrumv1alpha1.AddonChart) string {
	if strings.HasPrefix(c.Ref, "oci://") {
		return c.Ref + ":" + c.Version
	}
	return c.Ref
}

func (f *Fetcher) fetch(ctx context.Context, c zaentrumv1alpha1.AddonChart) ([]byte, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	if strings.HasPrefix(c.Ref, "oci://") {
		return f.fetchOCI(ctx, c)
	}
	data, err := f.fetchHTTPS(ctx, c.Ref)
	return data, nil, err
}

// fetchHTTPS GETs a chart archive. Redirects are followed only to https, so a
// link cannot be bent onto another scheme.
func (f *Fetcher) fetchHTTPS(ctx context.Context, link string) ([]byte, error) {
	client := &http.Client{
		Transport: f.transport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s:// refused: chart archives are fetched over https only", req.URL.Scheme)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch chart: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch chart: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch chart %s: %s", link, resp.Status)
	}
	if resp.ContentLength > MaxArchiveSize {
		return nil, errArchiveTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxArchiveSize+1))
	if err != nil {
		return nil, fmt.Errorf("fetch chart: %w", err)
	}
	if len(data) > MaxArchiveSize {
		return nil, errArchiveTooLarge
	}
	return data, nil
}

var errArchiveTooLarge = fmt.Errorf("chart archive is larger than %d MiB", MaxArchiveSize>>20)

// fetchOCI pulls a chart with the Helm registry client. It returns the chart
// layer and, as an alias digest, the manifest digest — the one `helm push`
// prints — so chart.digest may pin either.
func (f *Fetcher) fetchOCI(ctx context.Context, c zaentrumv1alpha1.AddonChart) ([]byte, []string, error) {
	// The registry client pulls on a background context of its own, so the
	// deadline and the response size bound ride on its HTTP client instead.
	httpClient := &http.Client{Transport: &boundedTransport{ctx: ctx, base: f.transport()}}
	// A resolver without credentials keeps every pull anonymous: whatever
	// registry credentials exist where the operator runs are never offered to
	// a registry an addon ref names.
	resolver := docker.NewResolver(docker.ResolverOptions{Client: httpClient, PlainHTTP: f.PlainHTTP})
	opts := []registry.ClientOption{
		registry.ClientOptHTTPClient(httpClient),
		registry.ClientOptResolver(resolver),
	}
	if f.PlainHTTP {
		opts = append(opts, registry.ClientOptPlainHTTP())
	}
	rc, err := registry.NewClient(opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("registry client: %w", err)
	}
	ref := strings.TrimPrefix(c.Ref, "oci://") + ":" + c.Version
	res, err := rc.Pull(ref, registry.PullOptWithChart(true))
	if err != nil {
		return nil, nil, fmt.Errorf("pull chart %s: %w", ref, err)
	}
	if len(res.Chart.Data) > MaxArchiveSize {
		return nil, nil, errArchiveTooLarge
	}
	return res.Chart.Data, []string{res.Manifest.Digest}, nil
}

// boundedTransport runs every request on the fetch context and refuses
// responses larger than MaxArchiveSize.
type boundedTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req.WithContext(t.ctx))
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > MaxArchiveSize {
		resp.Body.Close()
		return nil, errArchiveTooLarge
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, left: MaxArchiveSize}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// Probe for one more byte: exactly MaxArchiveSize is fine.
		var one [1]byte
		if n, _ := b.ReadCloser.Read(one[:]); n > 0 {
			return 0, errArchiveTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

func (f *Fetcher) transport() http.RoundTripper {
	if f.Transport != nil {
		return f.Transport
	}
	return http.DefaultTransport
}

func (f *Fetcher) ttl() time.Duration {
	if f.TTL > 0 {
		return f.TTL
	}
	return DefaultRefTTL
}

func (f *Fetcher) cachedDigest(digest string) *Archive {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.archives[digest]; e != nil {
		return e.archive
	}
	return nil
}

// cachedRef returns the archive last fetched for key; allowStale ignores the TTL.
func (f *Fetcher) cachedRef(key string, allowStale bool) *Archive {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.refs[key]
	if !ok || (!allowStale && time.Since(r.at) > f.ttl()) {
		return nil
	}
	if e := f.archives[r.digest]; e != nil {
		return e.archive
	}
	return nil
}

func (f *Fetcher) store(key string, a *Archive, aliases []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.archives == nil {
		f.archives = map[string]*cachedArchive{}
		f.refs = map[string]refEntry{}
	}
	now := time.Now()
	e := &cachedArchive{archive: a, at: now}
	f.archives[a.Digest] = e
	for _, d := range aliases {
		f.archives[d] = e
	}
	f.refs[key] = refEntry{digest: a.Digest, at: now}
	f.evict()
}

// evict drops the oldest archives until the cache fits maxCachedBytes. Must be
// called with f.mu held.
func (f *Fetcher) evict() {
	for {
		var total int
		var oldest *cachedArchive
		seen := map[*cachedArchive]bool{}
		for _, e := range f.archives {
			if seen[e] {
				continue
			}
			seen[e] = true
			total += len(e.archive.Data)
			if oldest == nil || e.at.Before(oldest.at) {
				oldest = e
			}
		}
		if total <= maxCachedBytes || len(seen) <= 1 {
			return
		}
		for d, e := range f.archives {
			if e == oldest {
				delete(f.archives, d)
			}
		}
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
