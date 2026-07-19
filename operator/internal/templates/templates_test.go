package templates

import (
	"fmt"
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
	assert.NotNil(t, find(t, objs, "Secret", "zaentrum-db"), "dev secrets rendered")
	assert.NotNil(t, find(t, objs, "PersistentVolumeClaim", "media"), "media PVC provisioned")
	for _, o := range objs {
		assert.Equal(t, "zaentrum", o.GetNamespace(), "namespace on %s/%s", o.GetKind(), o.GetName())
	}
}

// demoCR mirrors the demo profile (values-demo.yaml).
func demoCR(ns string) *zaentrumv1alpha1.Zaentrum {
	z := base(ns)
	z.Spec.Hostname = "zaentrum.demo.nalet.cloud"
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
	assert.Contains(t, fmt.Sprintf("%v", dep.Object),
		"https://zaentrum.demo.nalet.cloud/auth/realms/zaentrum", "https issuer in env")
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
	z.Spec.Hostname = "zaentrum.beta.nalet.cloud"
	z.Spec.Identity.Mode = "external"
	z.Spec.Identity.Issuer = "https://sso.nalet.cloud/realms/nalet"
	z.Spec.Features.Pipeline = true
	no, yes := false, true
	z.Spec.Routing.ProvisionIngress = &no
	z.Spec.Routing.ProvisionRoutes = &yes
	z.Spec.Routing.Mode = "subdomains"
	z.Spec.Routing.Hosts.Chino = "chino.beta.nalet.cloud"
	z.Spec.EventStreaming.Mode = "external"
	z.Spec.EventStreaming.Bootstrap = "platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093"
	z.Spec.EventStreaming.CertSecret = "kafka-mtls"
	z.Spec.EventStreaming.TopicPrefix = "zaentrum-beta."
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.Katalog = "katalog_beta"
	z.Spec.Databases.Chino = "chino_beta"
	z.Spec.Databases.Portal = "portal_beta"
	z.Spec.Databases.External.Host = "postgres.nalet.cloud"
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
	assert.Contains(t, blob, "postgres.nalet.cloud:5432/chino_beta?sslmode=require", "shared DSN")
	assert.Contains(t, blob, "kafka-mtls", "cert secret mounted")

	web := find(t, objs, "Deployment", "chino-web")
	require.NotNil(t, web)
	assert.Contains(t, fmt.Sprintf("%v", web.Object), "BASE_PATH value:/", "SPA at / on the subdomain")
}
