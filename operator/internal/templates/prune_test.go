package templates

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The operator removes an object of the platform once its render no longer
// carries it — unless it is of a kind that holds data or is a run. So every
// kind the chart renders, in every profile, is decided: one of PruneKinds or
// one of KeptKinds, never both, and a kind the chart starts to render fails
// here until it is one of them.
func TestEveryKindTheChartRendersIsPrunedOrKept(t *testing.T) {
	prune, kept := map[schema.GroupKind]bool{}, map[schema.GroupKind]bool{}
	for _, gvk := range PruneKinds {
		prune[gvk.GroupKind()] = true
	}
	for _, gk := range KeptKinds {
		kept[gk] = true
		assert.False(t, prune[gk], "%s is removed and kept", gk)
	}
	for _, gk := range []schema.GroupKind{{Kind: "PersistentVolumeClaim"}, {Kind: "Secret"}, {Kind: "ConfigMap"}, {Group: "batch", Kind: "Job"}} {
		assert.True(t, kept[gk], "%s holds data or is a run: it is never removed", gk)
	}

	yes := true
	tls := base("zaentrum")
	tls.Spec.Hostname = "media.example.org"
	tls.Spec.TLS = &zaentrumv1alpha1.TLSSpec{IssuerRef: &zaentrumv1alpha1.TLSIssuerRef{Name: "letsencrypt"}}
	tlsValues := NewValues(tls)
	tlsValues.CertManager = true
	ext := base("zaentrum-beta")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	ext.Spec.Databases.Mode = "external"
	ext.Spec.Databases.External.Host = "postgres.example.org"
	ext.Spec.EventStreaming.Mode = "external"
	ext.Spec.EventStreaming.Bootstrap = "kafka.example.org:9093"
	ext.Spec.Routing.ProvisionRoutes = &yes
	ext.Spec.Routing.Mode = "subdomains"
	ext.Spec.Routing.Hosts.Chino = "chino.example.org"
	pipeline := base("zaentrum")
	pipeline.Spec.Features.Pipeline = true
	demo := NewValues(demoCR("zaentrum-demo"))
	demo.OpenShift = true

	renders := map[string][]*unstructured.Unstructured{
		"self-host":      renderCR(t, base("zaentrum")),
		"pipeline":       renderCR(t, pipeline),
		"external":       renderCR(t, ext),
		"plain helm":     helmRender(t, map[string]interface{}{"features": map[string]interface{}{"pipeline": true}}),
		"seed jobs helm": helmRender(t, map[string]interface{}{"jobs": map[string]interface{}{"seed": true}}),
	}
	for name, v := range map[string]Values{"demo": demo, "cert-manager": tlsValues} {
		objs, err := Render(v)
		require.NoError(t, err, name)
		renders[name] = objs
	}
	seen := map[schema.GroupKind]bool{}
	for name, objs := range renders {
		platform, _ := SplitHooks(objs)
		for _, o := range platform {
			gk := o.GroupVersionKind().GroupKind()
			seen[gk] = true
			assert.True(t, prune[gk] != kept[gk], "%s: %s/%s is of %s, which is neither removed nor kept", name, o.GetKind(), o.GetName(), gk)
		}
	}
	// What each list names, some profile renders: no kind decided for nothing.
	for gk := range prune {
		assert.True(t, seen[gk], "PruneKinds names %s, which no profile renders", gk)
	}
}

// keys are the Kind/name of what a render applies, its hooks left out.
func keys(objs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	platform, _ := SplitHooks(objs)
	out := map[string]*unstructured.Unstructured{}
	for _, o := range platform {
		out[o.GetKind()+"/"+o.GetName()] = o
	}
	return out
}

// Turned off, the media pipeline's workers — the analyzer, the transcoder,
// the packager and katalog-ingest, each a Deployment and its Service — are
// what the render no longer carries, all of kinds the operator removes; and
// nothing else changes but what is rendered either way.
func TestTurningThePipelineOffDropsItsWorkers(t *testing.T) {
	for _, encoder := range []string{"gpu", "cpu"} {
		on := demoCR("zaentrum-demo")
		on.Spec.Pipeline.Encoder = encoder
		off := demoCR("zaentrum-demo")
		off.Spec.Features.Pipeline = false
		von, voff := NewValues(on), NewValues(off)
		von.OpenShift, voff.OpenShift = true, true
		rendered := func(v Values) map[string]*unstructured.Unstructured {
			objs, err := Render(v)
			require.NoError(t, err)
			return keys(objs)
		}
		with, without := rendered(von), rendered(voff)

		var dropped []string
		for k, o := range with {
			if _, ok := without[k]; ok {
				continue
			}
			dropped = append(dropped, k)
			removable := false
			for _, gvk := range PruneKinds {
				removable = removable || gvk.GroupKind() == o.GroupVersionKind().GroupKind()
			}
			assert.True(t, removable, "%s: %s is not of a kind the operator removes", encoder, k)
		}
		sort.Strings(dropped)
		assert.Equal(t, []string{
			"Deployment/analyzer", "Deployment/katalog-ingest", "Deployment/packager", "Deployment/transcoder",
			"Service/analyzer", "Service/katalog-ingest", "Service/packager", "Service/transcoder",
		}, dropped, encoder)
		for k := range without {
			assert.Contains(t, with, k, "%s: turning the pipeline off adds %s", encoder, k)
		}
	}
}
