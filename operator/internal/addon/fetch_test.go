package addon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// archiveServer serves the example archive over TLS at /example-0.1.0.tgz and
// counts the requests it answers.
func archiveServer(t *testing.T, archive []byte, fail *atomic.Bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case fail != nil && fail.Load():
			w.WriteHeader(http.StatusBadGateway)
		case r.URL.Path == "/example-0.1.0.tgz":
			_, _ = w.Write(archive)
		case r.URL.Path == "/to-http":
			http.Redirect(w, r, "http://"+r.Host+"/example-0.1.0.tgz", http.StatusFound)
		case r.URL.Path == "/huge.tgz":
			_, _ = w.Write(bytes.Repeat([]byte{0}, MaxArchiveSize+1))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchHTTPSArchive(t *testing.T) {
	archive := exampleArchive(t)
	srv, hits := archiveServer(t, archive, nil)
	f := &Fetcher{Transport: srv.Client().Transport}
	ref := zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/example-0.1.0.tgz", Version: "ignored"}

	a, err := f.Fetch(context.Background(), ref, false)
	require.NoError(t, err)
	assert.Equal(t, archive, a.Data)
	assert.Equal(t, Digest(archive), a.Digest)

	_, err = f.Fetch(context.Background(), ref, false)
	require.NoError(t, err)
	assert.EqualValues(t, 1, hits.Load(), "an unpinned ref is served from the cache within its TTL")

	_, err = f.Fetch(context.Background(), ref, true)
	require.NoError(t, err)
	assert.EqualValues(t, 2, hits.Load(), "a changed spec fetches fresh")
}

func TestFetchDigestPin(t *testing.T) {
	archive := exampleArchive(t)
	var fail atomic.Bool
	srv, hits := archiveServer(t, archive, &fail)
	f := &Fetcher{Transport: srv.Client().Transport}
	ref := zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/example-0.1.0.tgz", Digest: Digest(archive)}

	_, err := f.Fetch(context.Background(), ref, true)
	require.NoError(t, err)
	fail.Store(true)
	a, err := f.Fetch(context.Background(), ref, true)
	require.NoError(t, err, "a pinned digest in the cache needs no network")
	assert.Equal(t, archive, a.Data)
	assert.EqualValues(t, 1, hits.Load())

	fail.Store(false)
	wrong := ref
	wrong.Digest = "sha256:" + strings.Repeat("0", 64)
	_, err = f.Fetch(context.Background(), wrong, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chart digest mismatch")
}

// A registry or web server blip serves the archive last fetched for the ref.
func TestFetchServesLastArchiveOnFailure(t *testing.T) {
	archive := exampleArchive(t)
	var fail atomic.Bool
	srv, _ := archiveServer(t, archive, &fail)
	f := &Fetcher{Transport: srv.Client().Transport}
	ref := zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/example-0.1.0.tgz"}

	_, err := f.Fetch(context.Background(), ref, true)
	require.NoError(t, err)
	fail.Store(true)
	a, err := f.Fetch(context.Background(), ref, true)
	require.NoError(t, err)
	assert.Equal(t, archive, a.Data)

	_, err = (&Fetcher{Transport: srv.Client().Transport}).Fetch(context.Background(), ref, true)
	assert.Error(t, err, "without an earlier fetch the failure surfaces")
}

func TestFetchHTTPSLimits(t *testing.T) {
	srv, _ := archiveServer(t, exampleArchive(t), nil)
	f := &Fetcher{Transport: srv.Client().Transport}

	_, err := f.Fetch(context.Background(), zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/to-http"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect to http:// refused")

	_, err = f.Fetch(context.Background(), zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/huge.tgz"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than 10 MiB")

	_, err = f.Fetch(context.Background(), zaentrumv1alpha1.AddonChart{Ref: srv.URL + "/missing.tgz"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")

	_, err = f.Fetch(context.Background(), zaentrumv1alpha1.AddonChart{Ref: "http://charts.example.org/example-0.1.0.tgz"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be an oci:// reference or an https:// link")
}

// ociRegistry is the smallest OCI distribution endpoint a Helm pull needs:
// one chart manifest by tag or digest, its config and its chart layer.
func ociRegistry(t *testing.T, repo, tag string, archive []byte) (*httptest.Server, string) {
	t.Helper()
	config := []byte(`{"apiVersion":"v2","name":"example","version":"0.1.0"}`)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},`+
		`"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`,
		Digest(config), len(config), Digest(archive), len(archive)))
	manifestDigest := Digest(manifest)
	blobs := map[string][]byte{Digest(config): config, Digest(archive): archive}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/v2/" + repo + "/"
		switch {
		case r.URL.Path == prefix+"manifests/"+tag || r.URL.Path == prefix+"manifests/"+manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case strings.HasPrefix(r.URL.Path, prefix+"blobs/"):
			blob, ok := blobs[strings.TrimPrefix(r.URL.Path, prefix+"blobs/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
			_, _ = w.Write(blob)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, manifestDigest
}

func TestFetchOCI(t *testing.T) {
	archive := exampleArchive(t)
	srv, manifestDigest := ociRegistry(t, "charts/example", "0.1.0", archive)
	host := strings.TrimPrefix(srv.URL, "http://")
	f := &Fetcher{PlainHTTP: true}
	ref := zaentrumv1alpha1.AddonChart{Ref: "oci://" + host + "/charts/example", Version: "0.1.0"}

	a, err := f.Fetch(context.Background(), ref, true)
	require.NoError(t, err)
	assert.Equal(t, archive, a.Data)
	assert.Equal(t, Digest(archive), a.Digest, "the digest is always the archive's")

	for name, pin := range map[string]string{"archive digest": Digest(archive), "manifest digest": manifestDigest} {
		pinned := ref
		pinned.Digest = pin
		_, err := (&Fetcher{PlainHTTP: true}).Fetch(context.Background(), pinned, true)
		assert.NoError(t, err, "chart.digest may pin the %s", name)
	}

	pinned := ref
	pinned.Digest = "sha256:" + strings.Repeat("1", 64)
	_, err = (&Fetcher{PlainHTTP: true}).Fetch(context.Background(), pinned, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chart digest mismatch")

	missing := ref
	missing.Version = "9.9.9"
	_, err = (&Fetcher{PlainHTTP: true}).Fetch(context.Background(), missing, true)
	assert.Error(t, err)
}

// Registry responses are bounded like archives: exactly the limit passes, one
// byte more fails, whether or not the response declares its length.
func TestBoundedTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := MaxArchiveSize
		if r.URL.Path == "/over" {
			size++
		}
		if r.URL.Query().Get("declare") != "" {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		_, _ = w.Write(bytes.Repeat([]byte{1}, size))
	}))
	defer srv.Close()
	client := &http.Client{Transport: &boundedTransport{ctx: context.Background(), base: http.DefaultTransport}}
	get := func(path string) error {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}
	assert.NoError(t, get("/exact"))
	assert.ErrorIs(t, get("/over"), errArchiveTooLarge)
	assert.ErrorContains(t, get("/over?declare=1"), "larger than 10 MiB")
}

func TestValidateChart(t *testing.T) {
	ok := []zaentrumv1alpha1.AddonChart{
		{Ref: "oci://ghcr.io/example/charts/example", Version: "1.2.0"},
		{Ref: "oci://localhost:5000/example", Version: "1.2.0+build.1"},
		{Ref: "https://charts.example.org/example-1.2.0.tgz"},
		{Ref: "https://charts.example.org/example-1.2.0.tgz", Digest: "sha256:" + strings.Repeat("a", 64)},
	}
	for _, c := range ok {
		assert.NoError(t, ValidateChart(c), c.Ref)
	}
	for want, c := range map[string]zaentrumv1alpha1.AddonChart{
		"chart.version is required":            {Ref: "oci://ghcr.io/example/charts/example"},
		"must not carry a tag or digest":       {Ref: "oci://ghcr.io/example/charts/example:1.2.0", Version: "1.2.0"},
		"names no chart repository":            {Ref: "oci://ghcr.io", Version: "1"},
		"must be an oci:// reference":          {Ref: "http://charts.example.org/example.tgz"},
		"is not a valid https URL":             {Ref: "https://"},
		"must be sha256:<64 lowercase hex":     {Ref: "https://charts.example.org/x.tgz", Digest: "sha256:ABC"},
		"must be an oci:// reference or an ht": {Ref: "example"},
	} {
		err := ValidateChart(c)
		if assert.Error(t, err, c.Ref) {
			assert.Contains(t, err.Error(), want)
		}
	}
}

func TestValidateName(t *testing.T) {
	assert.NoError(t, ValidateName("example"))
	assert.NoError(t, ValidateName(strings.Repeat("a", MaxNameLength)))
	assert.Error(t, ValidateName(strings.Repeat("a", MaxNameLength+1)))
	assert.Error(t, ValidateName("Example"))
	assert.Error(t, ValidateName("example.addon"))
}
