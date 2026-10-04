package controller

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

var (
	deploymentKind = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	serviceKind    = schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	routeKind      = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
	certKind       = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
)

// metaOf is an object's metadata as a metadata-only list returns it: of
// gvk, in the Zaentrum's namespace, labelled for it and controlled by it,
// unless edit says otherwise.
func metaOf(z *zaentrumv1alpha1.Zaentrum, gvk schema.GroupVersionKind, name string, edit ...func(*metav1.PartialObjectMetadata)) metav1.PartialObjectMetadata {
	o := metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: z.Namespace, UID: types.UID(name + "-uid"),
		Labels: map[string]string{templates.LabelPlatform: platformLabel(z)},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: z.Name,
			UID: z.UID, Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true)}},
	}}
	o.SetGroupVersionKind(gvk)
	for _, e := range edit {
		e(&o)
	}
	return o
}

func names(objs []metav1.PartialObjectMetadata) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Kind+"/"+o.Name)
	}
	return out
}

// What the operator removes is what it applied for this Zaentrum and the
// render no longer carries: an object of a kind it removes, in its namespace,
// labelled for it, controlled by it — and nothing else. One case per rule.
func TestStaleIsOnlyWhatTheOperatorAppliedAndNoLongerRenders(t *testing.T) {
	z := verifyCR()
	rendered := []*unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "chino-api", "namespace": z.Namespace},
	}}}
	now := metav1.Now()
	live := []metav1.PartialObjectMetadata{
		// Gone from the render: removed.
		metaOf(z, deploymentKind, "analyzer"),
		metaOf(z, serviceKind, "analyzer"),
		metaOf(z, routeKind, "zaentrum-demo-old"),
		// Still rendered.
		metaOf(z, deploymentKind, "chino-api"),
		// What the operator did not apply for this Zaentrum.
		metaOf(z, deploymentKind, "unlabelled", func(o *metav1.PartialObjectMetadata) { o.Labels = nil }),
		metaOf(z, deploymentKind, "another-zaentrums", func(o *metav1.PartialObjectMetadata) { o.Labels[templates.LabelPlatform] = "elsewhere" }),
		metaOf(z, deploymentKind, "an-addons", func(o *metav1.PartialObjectMetadata) {
			o.OwnerReferences = []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "ZaentrumAddon", Name: "example",
				UID: "addon-uid", Controller: ptrTo(true)}}
		}),
		metaOf(z, deploymentKind, "owned-not-controlled", func(o *metav1.PartialObjectMetadata) { o.OwnerReferences[0].Controller = nil }),
		metaOf(z, deploymentKind, "nobodys", func(o *metav1.PartialObjectMetadata) { o.OwnerReferences = nil }),
		metaOf(z, deploymentKind, "elsewhere", func(o *metav1.PartialObjectMetadata) { o.Namespace = "another-namespace" }),
		// On its way out already.
		metaOf(z, deploymentKind, "deleting", func(o *metav1.PartialObjectMetadata) { o.DeletionTimestamp = &now }),
		// Kept: marked so, the bundled Postgres, and the kinds that hold data
		// or are runs.
		metaOf(z, serviceKind, "kept", func(o *metav1.PartialObjectMetadata) {
			o.Annotations = map[string]string{"helm.sh/resource-policy": "keep"}
		}),
		metaOf(z, deploymentKind, "postgres"),
		metaOf(z, schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, "postgres-data"),
		metaOf(z, schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, "zaentrum-db"),
		metaOf(z, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "keycloak-realm"),
		metaOf(z, schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, "zaentrum-realm-x1y2z"),
	}
	assert.Equal(t, []string{"Deployment/analyzer", "Route/zaentrum-demo-old", "Service/analyzer"}, names(stale(z, live, rendered)))

	// A Service of the same name as a rendered Deployment is not rendered.
	assert.Equal(t, []string{"Service/chino-api"}, names(stale(z, []metav1.PartialObjectMetadata{metaOf(z, serviceKind, "chino-api")}, rendered)))
}

// pipelineWorkers are what the pipeline renders: a Deployment and a Service
// each.
var pipelineWorkers = []string{
	"Deployment/analyzer", "Deployment/katalog-ingest", "Deployment/packager", "Deployment/transcoder",
	"Service/analyzer", "Service/katalog-ingest", "Service/packager", "Service/transcoder",
}

// exists says whether Kind/name is in the platform's namespace.
func (e *platformEnv) exists(kindName string) bool {
	e.t.Helper()
	kind, name, _ := strings.Cut(kindName, "/")
	gvk := map[string]schema.GroupVersionKind{
		"Deployment": deploymentKind, "Service": serviceKind, "ServiceAccount": {Version: "v1", Kind: "ServiceAccount"},
		"CronJob": {Group: "batch", Version: "v1", Kind: "CronJob"}, "PersistentVolumeClaim": {Version: "v1", Kind: "PersistentVolumeClaim"},
		"ConfigMap": {Version: "v1", Kind: "ConfigMap"}, "Secret": {Version: "v1", Kind: "Secret"},
		"Ingress": {Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
	}[kind]
	require.NotEmpty(e.t, gvk.Kind, kind)
	o := &metav1.PartialObjectMetadata{}
	o.SetGroupVersionKind(gvk)
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: e.key.Namespace, Name: name}, o)
	if err != nil {
		require.True(e.t, isGone(err), "%s: %v", kindName, err)
		return false
	}
	return true
}

// platform is the render the reconciler makes of the Zaentrum as it stands,
// its hooks left out.
func (e *platformEnv) platform() []*unstructured.Unstructured {
	e.t.Helper()
	objs, err := templates.Render(templates.NewValues(e.zaentrum()))
	require.NoError(e.t, err)
	platform, _ := templates.SplitHooks(objs)
	return platform
}

func prunedCondition(z *zaentrumv1alpha1.Zaentrum) *metav1.Condition {
	return meta.FindStatusCondition(z.Status.Conditions, condTypePruned)
}

// Turned off, the pipeline's workers go: a dry run first names exactly them
// and removes nothing; the next pass removes them, and only them, and the
// Pruned condition says so, and when — and keeps saying so while nothing more
// goes.
func TestPruneRemovesThePipelineOnceItIsOff(t *testing.T) {
	z := verifyCR()
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = "cpu"
	e := newPlatformEnv(t, z, nil)
	e.reconcile()
	for _, w := range pipelineWorkers {
		assert.True(t, e.exists(w), "%s applied", w)
	}
	c := prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, "NothingToRemove", c.Reason)

	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Features.Pipeline = false })

	removed, err := e.r.prune(context.Background(), e.zaentrum(), e.platform(), true)
	require.NoError(t, err)
	assert.Equal(t, pipelineWorkers, removed, "the dry run names what goes")
	for _, w := range pipelineWorkers {
		assert.True(t, e.exists(w), "a dry run removes nothing: %s", w)
	}

	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	for _, w := range pipelineWorkers {
		assert.False(t, e.exists(w), "%s is gone", w)
	}
	for _, kept := range []string{"Deployment/chino-api", "Deployment/postgres", "Service/katalog-manager-api", "Ingress/zaentrum",
		"PersistentVolumeClaim/media", "ConfigMap/katalog-schema", "Secret/zaentrum-db"} {
		assert.True(t, e.exists(kept), "%s stays", kept)
	}
	c = prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "Removed", c.Reason)
	assert.Equal(t, "2026-10-04 18:01 UTC: removed "+strings.Join(pipelineWorkers, ", ")+", which the platform no longer renders", c.Message)

	e.clock = e.clock.Add(time.Minute)
	e.reconcile()
	assert.Equal(t, c.Message, prunedCondition(e.zaentrum()).Message, "the last removal stays named")
	removed, err = e.r.prune(context.Background(), e.zaentrum(), e.platform(), true)
	require.NoError(t, err)
	assert.Empty(t, removed)
}

// What holds data stays when the render stops carrying it: the claims of the
// database, the backups and the library, the ConfigMaps, the Secrets, and the
// bundled Postgres itself, whose data on an emptyDir is in its pod. What only
// runs or serves goes with it — Kafka, Keycloak, the backups' schedule, the
// Postgres's Service.
func TestPruneKeepsWhatHoldsData(t *testing.T) {
	e := newPlatformEnv(t, verifyCR(), nil)
	e.reconcile()
	for _, o := range []string{"PersistentVolumeClaim/postgres-data", "PersistentVolumeClaim/backups", "PersistentVolumeClaim/media",
		"CronJob/zaentrum-backup", "Deployment/kafka", "Deployment/keycloak", "ConfigMap/keycloak-realm", "ConfigMap/postgres-initdb"} {
		require.True(t, e.exists(o), "%s applied", o)
	}

	no := false
	e.change(func(z *zaentrumv1alpha1.Zaentrum) {
		z.Spec.Databases.Mode = "external"
		z.Spec.Databases.External.Host = "postgres.example.org"
		z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
		z.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
		z.Spec.Features.Kafka = false
		z.Spec.Storage.ProvisionMedia = &no
		z.Spec.Backup.Enabled = &no
	})
	e.reconcile()

	for _, o := range []string{"PersistentVolumeClaim/postgres-data", "PersistentVolumeClaim/backups", "PersistentVolumeClaim/media",
		"ConfigMap/keycloak-realm", "ConfigMap/postgres-initdb", "Secret/zaentrum-db", "Deployment/postgres"} {
		assert.True(t, e.exists(o), "%s stays", o)
	}
	for _, o := range []string{"Deployment/kafka", "Service/kafka", "ServiceAccount/kafka", "Deployment/keycloak", "Service/keycloak",
		"Service/postgres", "CronJob/zaentrum-backup"} {
		assert.False(t, e.exists(o), "%s goes", o)
	}
	c := prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, "Removed", c.Reason)
	for _, kept := range []string{"PersistentVolumeClaim", "ConfigMap", "Secret", "Deployment/postgres"} {
		assert.NotContains(t, c.Message, kept, "the condition names only what went")
	}
}

// What the operator did not apply for this Zaentrum stays, whatever it is:
// one it applied before it labelled, one with another Zaentrum's label, an
// addon's, a person's.
func TestPruneLeavesWhatTheOperatorDidNotApply(t *testing.T) {
	z := verifyCR()
	controlled := []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: z.Name, UID: z.UID, Controller: ptrTo(true)}}
	dep := func(name string, labels map[string]string, owners []metav1.OwnerReference) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace, Labels: labels, OwnerReferences: owners}}
	}
	ours := map[string]string{templates.LabelPlatform: "zaentrum"}
	e := newPlatformEnv(t, z, nil,
		dep("from-before-the-label", map[string]string{"app.kubernetes.io/part-of": "zaentrum"}, controlled),
		dep("another-zaentrums", map[string]string{templates.LabelPlatform: "elsewhere"}, controlled),
		dep("an-addons", ours, []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "ZaentrumAddon", Name: "example",
			UID: "addon-uid", Controller: ptrTo(true)}}),
		dep("a-persons", ours, nil),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "a-persons", Namespace: z.Namespace}},
	)
	e.reconcile()
	e.reconcile()
	for _, o := range []string{"Deployment/from-before-the-label", "Deployment/another-zaentrums", "Deployment/an-addons",
		"Deployment/a-persons", "Service/a-persons"} {
		assert.True(t, e.exists(o), "%s stays", o)
	}
	c := prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "NothingToRemove", c.Reason)
}

// A removal that fails says so in the condition, fails nothing else, and is
// tried again on the next pass.
func TestPruneThatFailsIsTriedAgain(t *testing.T) {
	z := verifyCR()
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = "cpu"
	refuse := true
	e := newPlatformEnv(t, z, &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if refuse && obj.GetName() == "transcoder" && obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			return errors.New("admission webhook refused it")
		}
		return c.Delete(ctx, obj, opts...)
	}})
	e.reconcile()
	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Features.Pipeline = false })
	e.reconcile()

	assert.True(t, e.exists("Deployment/transcoder"))
	assert.False(t, e.exists("Deployment/analyzer"), "the rest goes")
	c := prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "Failed", c.Reason)
	assert.Contains(t, c.Message, "remove Deployment/transcoder: admission webhook refused it")
	assert.Contains(t, c.Message, "removed Deployment/analyzer")

	refuse = false
	e.reconcile()
	assert.False(t, e.exists("Deployment/transcoder"))
	c = prunedCondition(e.zaentrum())
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Contains(t, c.Message, "removed Deployment/transcoder")
}

// A kind the cluster does not serve — Routes off OpenShift, Certificates
// without cert-manager — has nothing of it to remove, and is not asked about
// again for a while: each such list costs a discovery.
func TestPruneSkipsKindsTheClusterDoesNotServe(t *testing.T) {
	lists := map[string]int{}
	e := newPlatformEnv(t, verifyCR(), &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if pl, ok := list.(*metav1.PartialObjectMetadataList); ok {
			gvk := pl.GroupVersionKind()
			lists[gvk.Kind]++
			for _, absent := range []schema.GroupVersionKind{routeKind, certKind} {
				if gvk.GroupKind() == (schema.GroupKind{Group: absent.Group, Kind: absent.Kind + "List"}) {
					return &meta.NoKindMatchError{GroupKind: absent.GroupKind(), SearchedVersions: []string{absent.Version}}
				}
			}
		}
		return c.List(ctx, list, opts...)
	}})
	e.reconcile()
	e.clock = e.clock.Add(5 * time.Minute)
	e.reconcile()
	assert.Equal(t, 1, lists["RouteList"], "asked once in ten minutes")
	assert.Equal(t, 1, lists["CertificateList"])
	assert.Equal(t, 2, lists["DeploymentList"], "a kind the cluster serves is listed every pass")
	c := prunedCondition(e.zaentrum())
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status, "a kind not served is no failure")

	e.clock = e.clock.Add(6 * time.Minute)
	e.reconcile()
	assert.Equal(t, 2, lists["RouteList"], "asked again after ten minutes")
}

// The prune reads only what is its own: each kind it removes, listed in the
// Zaentrum's namespace by the Zaentrum's label, metadata alone — never another
// namespace, never what carries no label.
func TestPruneListsOnlyItsOwnObjects(t *testing.T) {
	listed := map[string]client.ListOptions{}
	e := newPlatformEnv(t, verifyCR(), &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if pl, ok := list.(*metav1.PartialObjectMetadataList); ok {
			var o client.ListOptions
			o.ApplyOptions(opts)
			listed[strings.TrimSuffix(pl.GroupVersionKind().Kind, "List")] = o
		}
		return c.List(ctx, list, opts...)
	}})
	e.reconcile()
	require.Len(t, listed, len(templates.PruneKinds), "every kind the prune removes, metadata only")
	for _, gvk := range templates.PruneKinds {
		o, ok := listed[gvk.Kind]
		require.True(t, ok, gvk.Kind)
		assert.Equal(t, verifyNS, o.Namespace, "%s: the Zaentrum's namespace", gvk.Kind)
		require.NotNil(t, o.LabelSelector, gvk.Kind)
		assert.Equal(t, templates.LabelPlatform+"=zaentrum", o.LabelSelector.String(), "%s: by its label", gvk.Kind)
	}
}

// The order the dry run and the condition name what goes is the kind's, then
// the name's.
func TestStaleIsSorted(t *testing.T) {
	z := verifyCR()
	live := []metav1.PartialObjectMetadata{metaOf(z, serviceKind, "b"), metaOf(z, deploymentKind, "z"), metaOf(z, serviceKind, "a"),
		metaOf(z, deploymentKind, "a")}
	got := names(stale(z, live, nil))
	want := append([]string{}, got...)
	sort.Strings(want)
	assert.Equal(t, want, got)
	assert.Equal(t, []string{"Deployment/a", "Deployment/z", "Service/a", "Service/b"}, got)
}
