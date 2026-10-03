package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// checkerRegistry answers Resolve as a registry would: an image it lacks is
// ErrNotFound, and a registry that is down fails every lookup.
type checkerRegistry struct {
	missing map[string]bool
	down    bool
	asked   []string
}

func (f *checkerRegistry) Resolve(_ context.Context, image string) (string, error) {
	f.asked = append(f.asked, image)
	switch {
	case f.down:
		return "", errors.New("dial tcp: i/o timeout")
	case f.missing[image]:
		return "", fmt.Errorf("manifest zaentrum/zae: %w (404 Not Found)", digest.ErrNotFound)
	}
	return image, nil
}

// checkerTests renders the platform at version and returns its test hooks.
func checkerTests(t *testing.T, version string) []*unstructured.Unstructured {
	t.Helper()
	z := verifyCR()
	z.Spec.Version = version
	objs, err := templates.Render(templates.NewValues(z))
	require.NoError(t, err)
	_, tests := templates.SplitHooks(objs)
	return tests
}

// podImages returns the verification Job's container images by name, and its
// fallback annotation.
func podImages(t *testing.T, tests []*unstructured.Unstructured) (map[string]string, string) {
	t.Helper()
	job := templates.VerifyJob(tests)
	require.NotNil(t, job)
	images := map[string]string{}
	for _, key := range []string{"containers", "initContainers"} {
		list, _, err := unstructured.NestedSlice(job.Object, "spec", "template", "spec", key)
		require.NoError(t, err)
		for _, c := range list {
			cm := c.(map[string]any)
			images[cm["name"].(string)] = cm["image"].(string)
		}
	}
	return images, job.GetAnnotations()[annotationVerifyCheckerFallback]
}

func TestCheckerFallsBackToLatestForATagZaeLacks(t *testing.T) {
	tests := checkerTests(t, "v9.9.9")
	before, _ := podImages(t, tests)
	require.Equal(t, "ghcr.io/zaentrum/zae:v9.9.9", before[templates.VerifyCheckContainer])

	note := checkerFallback(context.Background(), &checkerRegistry{missing: map[string]bool{"ghcr.io/zaentrum/zae:v9.9.9": true}}, tests)
	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/zae:latest", after[templates.VerifyCheckContainer])
	assert.Equal(t, "v9.9.9", tag, "the Job says which tag was missing")
	assert.Contains(t, note, "v9.9.9")
	for name, image := range before {
		if name != templates.VerifyCheckContainer {
			assert.Equal(t, image, after[name], "the other containers keep their images: %s", name)
		}
	}
}

func TestCheckerKeepsAReleaseTagThatExists(t *testing.T) {
	tests := checkerTests(t, "v1.0.0")
	reg := &checkerRegistry{}
	assert.Empty(t, checkerFallback(context.Background(), reg, tests))
	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/zae:v1.0.0", after[templates.VerifyCheckContainer])
	assert.Empty(t, tag)
	assert.Equal(t, []string{"ghcr.io/zaentrum/zae:v1.0.0"}, reg.asked)
}

func TestCheckerLeavesLatestAlone(t *testing.T) {
	tests := checkerTests(t, "latest")
	reg := &checkerRegistry{missing: map[string]bool{"ghcr.io/zaentrum/zae:latest": true}}
	assert.Empty(t, checkerFallback(context.Background(), reg, tests))
	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/zae:latest", after[templates.VerifyCheckContainer])
	assert.Empty(t, tag)
	assert.Empty(t, reg.asked, "latest has nothing to fall back to")
}

func TestCheckerKeepsTheTagWhenTheRegistryCannotBeAsked(t *testing.T) {
	tests := checkerTests(t, "v9.9.9")
	assert.Empty(t, checkerFallback(context.Background(), &checkerRegistry{down: true}, tests))
	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/zae:v9.9.9", after[templates.VerifyCheckContainer],
		"a registry that does not answer says nothing about the tag")
	assert.Empty(t, tag)
}

func TestCheckerLeavesAPinnedImageAlone(t *testing.T) {
	tests := checkerTests(t, "v9.9.9")
	job := templates.VerifyJob(tests)
	containers, _, _ := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	pinned := "ghcr.io/zaentrum/zae@sha256:" + sha256Hex("zae")
	for _, c := range containers {
		if cm := c.(map[string]any); cm["name"] == templates.VerifyCheckContainer {
			cm["image"] = pinned
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(job.Object, containers, "spec", "template", "spec", "containers"))
	reg := &checkerRegistry{missing: map[string]bool{pinned: true}}
	assert.Empty(t, checkerFallback(context.Background(), reg, tests))
	after, _ := podImages(t, tests)
	assert.Equal(t, pinned, after[templates.VerifyCheckContainer])
	assert.Empty(t, reg.asked)
}

func TestCheckerWithoutAVerificationJob(t *testing.T) {
	reg := &checkerRegistry{}
	assert.Empty(t, checkerFallback(context.Background(), reg, nil))
	assert.Empty(t, reg.asked)
}

// A run that checks with zae:latest says so when it finishes.
func TestVerifySaysWhenItCheckedWithLatest(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v9.9.9"
	e := newVerifyEnv(t, z, nil)
	require.NotEmpty(t, checkerFallback(context.Background(),
		&checkerRegistry{missing: map[string]bool{"ghcr.io/zaentrum/zae:v9.9.9": true}}, e.tests))

	require.True(t, e.pass(true))
	job := e.job(e.v().Job)
	assert.Equal(t, "v9.9.9", job.Annotations[annotationVerifyCheckerFallback], "the run's Job keeps the mark")
	assert.Equal(t, "ghcr.io/zaentrum/zae:latest", job.Spec.Template.Spec.Containers[0].Image)

	e.end(0, "Completed", report(healthy...))
	e.pass(true)
	v := e.v()
	assert.Equal(t, zaentrumv1alpha1.VerificationPassed, v.Result)
	assert.Equal(t, "5 of 5 checks passed; warnings: image registry; checked with zae:latest, as no zae image is tagged v9.9.9", v.Message)
}

// Only the zae image falls back: a check image someone set to another
// repository is theirs to keep.
func TestCheckerLeavesAnotherImageAlone(t *testing.T) {
	tests := checkerTests(t, "v9.9.9")
	job := templates.VerifyJob(tests)
	containers, _, _ := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		if cm := c.(map[string]any); cm["name"] == templates.VerifyCheckContainer {
			cm["image"] = "ghcr.io/zaentrum/own-checks:v9.9.9"
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(job.Object, containers, "spec", "template", "spec", "containers"))
	reg := &checkerRegistry{missing: map[string]bool{"ghcr.io/zaentrum/own-checks:v9.9.9": true}}
	assert.Empty(t, checkerFallback(context.Background(), reg, tests))
	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/own-checks:v9.9.9", after[templates.VerifyCheckContainer])
	assert.Empty(t, tag)
}

// toRegistry sends every request to the test registry, whatever host it names:
// the resolver asks ghcr.io as it would in a cluster, and the test answers.
type toRegistry struct{ host string }

func (t toRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Host, r.Host = t.host, t.host
	return http.DefaultTransport.RoundTrip(r)
}

// The reconcile's pinning pass falls back first and pins after: a platform at a
// tag the zae image lacks checks with zae:latest, pinned to latest's digest.
func TestPinDigestsChecksWithLatestForATagZaeLacks(t *testing.T) {
	const latestDigest = "sha256:" + "5555555555555555555555555555555555555555555555555555555555555555"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/zaentrum/zae/manifests/latest" {
			w.Header().Set("Docker-Content-Digest", latestDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	rv := digest.New(nil)
	rv.Scheme = "http"
	rv.HTTP = &http.Client{Transport: toRegistry{host: srv.Listener.Addr().String()}}

	z := verifyCR()
	z.Spec.Version = "v9.9.9"
	objs, err := templates.Render(templates.NewValues(z))
	require.NoError(t, err)
	platform, tests := templates.SplitHooks(objs)
	r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(selfScheme(t)).WithObjects(z).Build(),
		PinDigests: true, Digest: rv}
	r.pinDigests(context.Background(), z, append(platform[:len(platform):len(platform)], tests...))

	after, tag := podImages(t, tests)
	assert.Equal(t, "ghcr.io/zaentrum/zae@"+latestDigest, after[templates.VerifyCheckContainer])
	assert.Equal(t, "v9.9.9", tag)
}
