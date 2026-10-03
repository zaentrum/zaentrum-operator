package templates

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// base returns a minimal CR; Kafka mirrors the CRD default (the API server sets
// it true — a struct built in-test must set it explicitly).
func base(ns string) *zaentrumv1alpha1.Zaentrum {
	z := &zaentrumv1alpha1.Zaentrum{}
	z.Name = "zaentrum"
	z.Namespace = ns
	z.Spec.Features.Kafka = true
	return z
}

func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

// helmRender renders the chart as `helm template` does: values.yaml under
// vals, no cluster to look anything up in.
func helmRender(t *testing.T, vals map[string]interface{}) []*unstructured.Unstructured {
	t.Helper()
	objs, err := render(vals, "zaentrum", false, nil)
	require.NoError(t, err)
	return objs
}

func count(objs []*unstructured.Unstructured, kind string) int {
	n := 0
	for _, o := range objs {
		if o.GetKind() == kind {
			n++
		}
	}
	return n
}

func replicas(t *testing.T, objs []*unstructured.Unstructured, name string) int64 {
	t.Helper()
	d := find(t, objs, "Deployment", name)
	require.NotNil(t, d, "Deployment %s", name)
	// The YAML decoder represents numbers as float64.
	n, found, err := unstructured.NestedFloat64(d.Object, "spec", "replicas")
	require.NoError(t, err)
	require.True(t, found, "Deployment %s has no spec.replicas", name)
	return int64(n)
}

// Self-host defaults: bundled infra + core apps, an Ingress (not Routes), dev
// secrets, no pipeline, every object namespaced.
func TestRenderSelfHost(t *testing.T) {
	objs, err := Render(NewValues(base("zaentrum")))
	require.NoError(t, err)
	require.NotEmpty(t, objs)

	for _, n := range []string{
		"postgres", "valkey", "kafka", "keycloak",
		"chino-api", "chino-stream", "chino-web",
		"katalog-api", "katalog-manager-api", "katalog-manager-ui",
		"portal-api", "zaentrum-portal",
	} {
		assert.NotNil(t, find(t, objs, "Deployment", n), "Deployment %s", n)
	}
	assert.Equal(t, 1, count(objs, "Ingress"), "self-host renders an Ingress")
	assert.Equal(t, 0, count(objs, "Route"), "no OpenShift Routes by default")
	assert.Nil(t, find(t, objs, "Deployment", "analyzer"), "pipeline off by default")
	assert.Zero(t, count(objs, "Secret"), "the operator makes the platform's Secrets itself; the render carries none")
	assert.NotNil(t, find(t, objs, "PersistentVolumeClaim", "media"), "media PVC provisioned")
	// The cluster assigns Keycloak's address from its own service range: a
	// pinned one fits only the cluster it was picked on (on k3s, 10.43.0.0/16,
	// the Service could not be created at all).
	kc := find(t, objs, "Service", "keycloak")
	require.NotNil(t, kc)
	_, pinned, _ := unstructured.NestedString(kc.Object, "spec", "clusterIP")
	assert.False(t, pinned, "the keycloak Service pins no clusterIP")
	for _, o := range objs {
		assert.Equal(t, "zaentrum", o.GetNamespace(), "namespace on %s/%s", o.GetKind(), o.GetName())
	}
}

// demoCR mirrors the demo profile (values-demo.yaml).
func demoCR(ns string) *zaentrumv1alpha1.Zaentrum {
	z := base(ns)
	z.Spec.Hostname = "zaentrum.demo.nalet.cloud" // neutrality-guard:allow
	z.Spec.Identity.IssuerScheme = "https"
	z.Spec.Identity.LoginTheme = "zaentrum"
	z.Spec.Features.Pipeline = true
	no, yes := false, true
	z.Spec.Storage.ProvisionMedia = &no
	z.Spec.Routing.ProvisionIngress = &no
	z.Spec.Routing.ProvisionRoutes = &yes
	z.Spec.Network.IssuerHostAliasIP = "77.109.148.13"
	z.Spec.Secrets.External = true
	z.Spec.PartOf = "zaentrum-demo"
	return z
}

// Demo profile: pipeline on, Routes not Ingress, external secrets (none rendered),
// external media PVC (none), https issuer + split-horizon hostAliases on validators.
func TestRenderDemoProfile(t *testing.T) {
	objs, err := Render(NewValues(demoCR("zaentrum-demo")))
	require.NoError(t, err)

	assert.NotNil(t, find(t, objs, "Deployment", "analyzer"), "pipeline on")
	assert.NotNil(t, find(t, objs, "Deployment", "transcoder"), "pipeline on")
	assert.Greater(t, count(objs, "Route"), 0, "OpenShift Routes")
	assert.Equal(t, 0, count(objs, "Ingress"), "no Ingress in demo")
	assert.Equal(t, 0, count(objs, "Secret"), "external secrets → none rendered")
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "media"), "external media PVC")

	dep := find(t, objs, "Deployment", "chino-api")
	require.NotNil(t, dep)
	sp, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec")
	_, hasHA := sp["hostAliases"]
	assert.True(t, hasHA, "chino-api carries split-horizon hostAliases")
	blob := fmt.Sprintf("%v", dep.Object)
	assert.Contains(t, blob,
		"https://zaentrum.demo.nalet.cloud/auth/realms/zaentrum", "https issuer in env") // neutrality-guard:allow
	// The extension seam is neutral core: chino-api points at portal-api, but the
	// rendered core carries NO acquisition/addon vocabulary.
	assert.Contains(t, blob, "PORTAL_BASE_URL", "chino-api wired to the portal registry")
	all := fmt.Sprintf("%v", objs)
	for _, forbidden := range []string{"acquire", "download-gateway", "qbittorrent", "wanted"} { // neutrality-guard:allow
		assert.NotContains(t, strings.ToLower(all), forbidden,
			"core render must not mention acquisition (%s)", forbidden)
	}
}

// spec.replicas overrides an app-tier Deployment; unlisted default to 1.
func TestReplicasOverride(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Replicas = map[string]int32{"chino-api": 3}
	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	assert.Equal(t, int64(3), replicas(t, objs, "chino-api"), "override applied")
	assert.Equal(t, int64(1), replicas(t, objs, "chino-web"), "unlisted defaults to 1")
}

// Shared-services beta profile: external identity + shared Kafka (mTLS, tenant
// topic prefix) + shared Postgres + a chino subdomain. Asserts the chart drops
// every bundled backer and wires the tenant endpoints through.
func TestRenderSharedBetaProfile(t *testing.T) {
	z := base("zaentrum-beta")
	z.Spec.Hostname = "zaentrum.beta.example.com"
	z.Spec.Identity.Mode = "external"
	z.Spec.Identity.Issuer = "https://sso.example.com/realms/nalet"
	z.Spec.Features.Pipeline = true
	no, yes := false, true
	z.Spec.Routing.ProvisionIngress = &no
	z.Spec.Routing.ProvisionRoutes = &yes
	z.Spec.Routing.Mode = "subdomains"
	z.Spec.Routing.Hosts.Chino = "chino.example.com"
	z.Spec.EventStreaming.Mode = "external"
	z.Spec.EventStreaming.Bootstrap = "platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093"
	z.Spec.EventStreaming.CertSecret = "kafka-mtls"
	z.Spec.EventStreaming.TopicPrefix = "zaentrum-beta."
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.Katalog = "katalog_beta"
	z.Spec.Databases.Chino = "chino_beta"
	z.Spec.Databases.Portal = "portal_beta"
	z.Spec.Databases.External.Host = "postgres.example.com"
	z.Spec.Databases.External.SSLMode = "require"
	z.Spec.Secrets.External = true
	_ = yes

	objs, err := Render(NewValues(z))
	require.NoError(t, err)

	assert.Nil(t, find(t, objs, "Deployment", "kafka"), "no bundled broker")
	assert.Nil(t, find(t, objs, "Deployment", "postgres"), "no bundled postgres")
	assert.Nil(t, find(t, objs, "Deployment", "keycloak"), "external identity")
	assert.NotNil(t, find(t, objs, "Route", "chino-sub-root"), "chino subdomain route")
	assert.NotNil(t, find(t, objs, "Route", "chino-sub-api"), "chino subdomain api route")

	dep := find(t, objs, "Deployment", "chino-api")
	require.NotNil(t, dep)
	blob := fmt.Sprintf("%v", dep.Object)
	assert.Contains(t, blob, "platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093", "shared bootstrap")
	assert.Contains(t, blob, "zaentrum-beta.", "tenant topic prefix")
	assert.Contains(t, blob, "postgres.example.com:5432/chino_beta?sslmode=require", "shared DSN")
	assert.Contains(t, blob, "kafka-mtls", "cert secret mounted")

	web := find(t, objs, "Deployment", "chino-web")
	require.NotNil(t, web)
	assert.Contains(t, fmt.Sprintf("%v", web.Object), "BASE_PATH value:/", "SPA at / on the subdomain")

	// External identity: no beta workload may reference the bundled-realm secret
	// (it is never rendered outside bundled mode → CreateContainerConfigError).
	// Workers mint client-credentials at the external issuer with the CI-provided
	// zaentrum-worker-oidc client instead.
	for _, name := range []string{"analyzer", "transcoder", "packager", "katalog-manager-api"} {
		d := find(t, objs, "Deployment", name)
		require.NotNil(t, d, name)
		blob := fmt.Sprintf("%v", d.Object)
		assert.NotContains(t, blob, "zaentrum-keycloak", "%s must not reference the bundled realm secret", name)
		assert.Contains(t, blob, "zaentrum-worker-oidc", "%s uses the external worker client", name)
	}
	an := fmt.Sprintf("%v", find(t, objs, "Deployment", "analyzer").Object)
	assert.Contains(t, an, "https://sso.example.com/realms/nalet/protocol/openid-connect/token",
		"worker token endpoint derived from the external issuer")

	// identity.clientId flows to /api/config's WEB client id — both SPAs
	// (chino-web + the portal shell) authenticate as it.
	z.Spec.Identity.ClientID = "chino-beta"
	objs2, err := Render(NewValues(z))
	require.NoError(t, err)
	api := fmt.Sprintf("%v", find(t, objs2, "Deployment", "chino-api").Object)
	assert.Contains(t, api, "OIDC_CLIENT_ID_WEB value:chino-beta", "CR clientId reaches /api/config")
	assert.Contains(t, api, "OIDC_CLIENT_ID_PORTAL value:chino-beta", "portal rides the per-instance client on a shared realm")
	pa := fmt.Sprintf("%v", find(t, objs2, "Deployment", "portal-api").Object)
	assert.Contains(t, pa, "CHINO_PUBLIC_URL value:https://chino.example.com/", "chino tile gets the subdomain origin")
	assert.Nil(t, find(t, objs2, "Route", "zaentrum-demo-auth"), "no bundled-keycloak /auth route in external identity")
	assert.NotNil(t, find(t, objs2, "Route", "zaentrum-demo-auth-callback"), "SPA callback route stays")
}

// On OpenShift the SCC gives every pod its user, and a fixed one would fall
// outside the namespace's range; anywhere else the kubelet must see a number
// to verify runAsNonRoot, so every pod that refuses root names 65532. Both
// hold for every workload the chart renders, the pipeline's and the
// verification Job's included.
func TestPodsNameAUserOnlyOffOpenShift(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Features.Pipeline = true
	for _, openShift := range []bool{false, true} {
		v := NewValues(z)
		v.OpenShift = openShift
		objs, err := Render(v)
		require.NoError(t, err)
		checked := 0
		for _, o := range objs {
			if o.GetKind() != "Deployment" && o.GetKind() != "Job" {
				continue
			}
			sc, found, _ := unstructured.NestedMap(o.Object, "spec", "template", "spec", "securityContext")
			if !found || sc["runAsNonRoot"] != true {
				continue
			}
			checked++
			raw, named, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "template", "spec", "securityContext", "runAsUser")
			user := asInt64(raw)
			if openShift {
				assert.False(t, named, "%s/%s names a user on OpenShift", o.GetKind(), o.GetName())
			} else {
				assert.True(t, named, "%s/%s names no user off OpenShift", o.GetKind(), o.GetName())
				assert.Equal(t, int64(65532), user, "%s/%s", o.GetKind(), o.GetName())
			}
		}
		assert.GreaterOrEqual(t, checked, 12, "every pod that refuses root (openShift=%v)", openShift)
	}
}

// asInt64 reads a number as the YAML decoder left it, whichever type that was.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// The media claim the chart creates fits any cluster: no volume of one cluster
// named in it, the default StorageClass unless one is set (an empty class
// would turn dynamic provisioning off), and the access mode the CR asks for —
// ReadWriteOnce by default, which local-path and one node can give.
func TestMediaClaimFitsAnyCluster(t *testing.T) {
	claim := func(z *zaentrumv1alpha1.Zaentrum) map[string]any {
		t.Helper()
		objs, err := Render(NewValues(z))
		require.NoError(t, err)
		pvc := find(t, objs, "PersistentVolumeClaim", "media")
		require.NotNil(t, pvc)
		spec, _, _ := unstructured.NestedMap(pvc.Object, "spec")
		return spec
	}

	spec := claim(base("zaentrum"))
	assert.Equal(t, []any{"ReadWriteOnce"}, spec["accessModes"])
	assert.NotContains(t, spec, "volumeName", "no volume of another cluster")
	assert.NotContains(t, spec, "storageClassName", "the cluster's default StorageClass")

	z := base("zaentrum")
	z.Spec.Storage.ClassName = "fast"
	z.Spec.Storage.MediaAccessMode = "ReadWriteMany"
	spec = claim(z)
	assert.Equal(t, []any{"ReadWriteMany"}, spec["accessModes"])
	assert.Equal(t, "fast", spec["storageClassName"])
}
