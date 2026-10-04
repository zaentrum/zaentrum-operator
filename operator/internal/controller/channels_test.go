package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

// A release channel is only as good as what its installs end up pulling. These
// tests reconcile real renders of the chart against a channel document and a
// registry they control: stable names a release, edge serves latest, and a push
// to main moves latest.

// tagRegistry stands in for ghcr.io: every manifest lookup the digest resolver
// makes lands here, and each tag answers with the digest it points at now, the
// same for every ghcr.io/zaentrum image. move is a push.
type tagRegistry struct {
	mu   sync.Mutex
	tags map[string]string
	srv  *httptest.Server
}

func newTagRegistry(t *testing.T, tags map[string]string) *tagRegistry {
	t.Helper()
	reg := &tagRegistry{tags: tags}
	reg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := strings.LastIndex(r.URL.Path, "/manifests/")
		if !strings.HasPrefix(r.URL.Path, "/v2/zaentrum/") || i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reg.mu.Lock()
		d, ok := reg.tags[r.URL.Path[i+len("/manifests/"):]]
		reg.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(reg.srv.Close)
	return reg
}

func (reg *tagRegistry) move(tag, d string) {
	reg.mu.Lock()
	reg.tags[tag] = d
	reg.mu.Unlock()
}

// resolver asks this registry whatever host an image names (toRegistry, in
// checker_test.go), and holds no pin past a pass, so a push shows on the next.
func (reg *tagRegistry) resolver() *digest.Resolver {
	rv := digest.New(nil)
	rv.TTL = 0
	rv.Scheme = "http"
	rv.HTTP = &http.Client{Transport: toRegistry{host: reg.srv.Listener.Addr().String()}}
	return rv
}

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// channelDocument serves releases.json until fail is set, then answers 500.
type channelDocument struct {
	mu   sync.Mutex
	body string
	fail bool
	srv  *httptest.Server
}

func newChannelDocument(t *testing.T, body string) *channelDocument {
	t.Helper()
	doc := &channelDocument{body: body}
	doc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		doc.mu.Lock()
		defer doc.mu.Unlock()
		if doc.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(doc.body))
	}))
	t.Cleanup(doc.srv.Close)
	return doc
}

func (doc *channelDocument) unreachable() {
	doc.mu.Lock()
	doc.fail = true
	doc.mu.Unlock()
}

// channelInstall is a Zaentrum on a channel, written as the sample is: version
// latest (follow the channel), manual updates.
func channelInstall(ns string, channel zaentrumv1alpha1.Channel, running string) *zaentrumv1alpha1.Zaentrum {
	z := &zaentrumv1alpha1.Zaentrum{ObjectMeta: metav1.ObjectMeta{
		Name: "zaentrum", Namespace: ns, UID: types.UID(ns + "-uid"),
	}}
	z.Spec.Channel = channel
	z.Spec.Version = "latest"
	z.Spec.Features.Kafka = true
	z.Status.CurrentVersion = running
	return z
}

func channelReconciler(t *testing.T, doc *channelDocument, reg *tagRegistry, installs ...*zaentrumv1alpha1.Zaentrum) (*ZaentrumReconciler, client.Client) {
	t.Helper()
	s := selfScheme(t)
	objs := []client.Object{managerPod("ghcr.io/zaentrum/operator:v0.1.0"), managerDeployment()}
	for _, z := range installs {
		objs = append(objs, z)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyAsCreateOrUpdate}).
		Build()
	return &ZaentrumReconciler{
		Client:      c,
		Scheme:      s,
		ReleasesURL: doc.srv.URL,
		Updates:     updates.Client{HTTP: doc.srv.Client()},
		PinDigests:  true,
		Digest:      reg.resolver(),
	}, c
}

func reconcileAll(t *testing.T, r *ZaentrumReconciler, installs ...*zaentrumv1alpha1.Zaentrum) {
	t.Helper()
	for _, z := range installs {
		_, err := r.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: z.Namespace, Name: z.Name}})
		require.NoError(t, err, z.Namespace)
	}
}

func installStatus(t *testing.T, c client.Client, ns string) zaentrumv1alpha1.ZaentrumStatus {
	t.Helper()
	var z zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "zaentrum"}, &z))
	return z.Status
}

// platformImages is every ghcr.io/zaentrum image the install's Deployments run,
// by container.
func platformImages(t *testing.T, c client.Client, ns string) map[string]string {
	t.Helper()
	var deps appsv1.DeploymentList
	require.NoError(t, c.List(context.Background(), &deps, client.InNamespace(ns)))
	out := map[string]string{}
	for _, d := range deps.Items {
		for _, ctr := range d.Spec.Template.Spec.Containers {
			if strings.HasPrefix(ctr.Image, "ghcr.io/zaentrum/") {
				out[d.Name+"/"+ctr.Name] = ctr.Image
			}
		}
	}
	require.NotEmpty(t, out, "the install in %s runs no ghcr.io/zaentrum image", ns)
	return out
}

// assertAllOn checks every platform image of the install is pinned to want.
func assertAllOn(t *testing.T, c client.Client, ns, want, why string) {
	t.Helper()
	for name, image := range platformImages(t, c, ns) {
		assert.True(t, strings.HasSuffix(image, "@"+want), "%s: %s runs %s — %s", ns, name, image, why)
	}
}

// A new install on stable runs the release stable names, and a push to main
// moves nothing in it: digest pinning re-resolves the tag its channel names, not
// latest. An install on edge runs latest and follows every push. A channel
// document that cannot be read keeps each where it is.
func TestStableRunsItsReleaseAndMainPushesMoveOnlyEdge(t *testing.T) {
	selfEnv(t)
	doc := newChannelDocument(t, `{"channels":{"stable":"v0.1.0","edge":"latest"}}`)
	release, before, after := digestOf('1'), digestOf('a'), digestOf('b')
	reg := newTagRegistry(t, map[string]string{"v0.1.0": release, updates.Latest: before})

	stable := channelInstall("stable", zaentrumv1alpha1.ChannelStable, "")
	edge := channelInstall("edge", zaentrumv1alpha1.ChannelEdge, "")
	r, c := channelReconciler(t, doc, reg, stable, edge)

	reconcileAll(t, r, stable, edge)
	assert.Equal(t, "v0.1.0", installStatus(t, c, "stable").CurrentVersion)
	assert.Empty(t, installStatus(t, c, "stable").AvailableUpdate)
	assertAllOn(t, c, "stable", release, "a new install on stable runs the release")
	assert.Equal(t, updates.Latest, installStatus(t, c, "edge").CurrentVersion)
	assertAllOn(t, c, "edge", before, "an install on edge runs latest")

	reg.move(updates.Latest, after) // a push to main
	reconcileAll(t, r, stable, edge)
	assert.Equal(t, "v0.1.0", installStatus(t, c, "stable").CurrentVersion)
	assertAllOn(t, c, "stable", release, "a push to main must not move the stable install")
	assertAllOn(t, c, "edge", after, "edge follows main push by push")

	doc.unreachable()
	reconcileAll(t, r, stable, edge)
	assert.Equal(t, "v0.1.0", installStatus(t, c, "stable").CurrentVersion)
	assertAllOn(t, c, "stable", release, "an unreadable channel document must not move the stable install to latest")
	assertAllOn(t, c, "edge", after, "edge stays on latest")
}

// The reference demo runs latest on stable, as every install did before stable
// named a release, in manual mode. The release does not move it: it keeps
// following latest push by push and is told that stable now serves v0.1.0.
func TestAnInstallOnLatestStaysThereWhenStableGetsARelease(t *testing.T) {
	selfEnv(t)
	doc := newChannelDocument(t, `{"channels":{"stable":"v0.1.0","edge":"latest"}}`)
	before, after := digestOf('a'), digestOf('b')
	reg := newTagRegistry(t, map[string]string{"v0.1.0": digestOf('1'), updates.Latest: before})

	demo := channelInstall("demo", zaentrumv1alpha1.ChannelStable, updates.Latest)
	r, c := channelReconciler(t, doc, reg, demo)

	reconcileAll(t, r, demo)
	assert.Equal(t, updates.Latest, installStatus(t, c, "demo").CurrentVersion)
	assert.Equal(t, "v0.1.0", installStatus(t, c, "demo").AvailableUpdate)
	assertAllOn(t, c, "demo", before, "manual mode keeps an install on what it runs")

	reg.move(updates.Latest, after)
	reconcileAll(t, r, demo)
	assertAllOn(t, c, "demo", after, "an install on latest follows main as it always did")
}
