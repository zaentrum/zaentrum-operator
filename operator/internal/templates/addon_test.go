package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// envValue returns the value of env var name on the first container of a
// rendered Deployment.
func envValue(t *testing.T, objs []*unstructured.Unstructured, deployment, name string) string {
	t.Helper()
	d := find(t, objs, "Deployment", deployment)
	require.NotNil(t, d, "Deployment %s", deployment)
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NotEmpty(t, containers)
	env, _, _ := unstructured.NestedSlice(containers[0].(map[string]interface{}), "env")
	for _, e := range env {
		m := e.(map[string]interface{})
		if m["name"] == name {
			v, _ := m["value"].(string)
			return v
		}
	}
	t.Fatalf("Deployment %s has no env %s", deployment, name)
	return ""
}

// Self-host defaults: the bundled broker, the derived issuer, "<namespace>-addons".
// Every fact must equal what the platform itself was rendered with.
func TestAddonPlatformValuesSelfHost(t *testing.T) {
	z := base("zaentrum")
	vals, err := AddonPlatformValues(z)
	require.NoError(t, err)

	assert.Equal(t, map[string]interface{}{
		"namespace":         "zaentrum",
		"hostname":          "zaentrum.localhost",
		"issuer":            "http://zaentrum.localhost/auth/realms/zaentrum",
		"issuerHostAliasIP": "",
		"imagePullSecrets":  []interface{}{},
		"partOf":            "zaentrum-addons",
		"events": map[string]interface{}{
			"brokers":     "kafka:9092",
			"topicPrefix": "stube.",
			"tlsSecret":   "",
		},
		"media": map[string]interface{}{"claimName": "media"},
	}, vals)

	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	assert.Equal(t, envValue(t, objs, "portal-api", "KAFKA_BROKERS"), vals["events"].(map[string]interface{})["brokers"])
	assert.Equal(t, envValue(t, objs, "portal-api", "OIDC_ISSUER"), vals["issuer"])
	assert.NotNil(t, find(t, objs, "PersistentVolumeClaim", "media"), "the claim addons are told about is the platform's")
}

// Shared-services profile: external issuer, shared cluster with mTLS and a
// tenant prefix, pull secrets, split-horizon alias and an explicit partOf.
func TestAddonPlatformValuesShared(t *testing.T) {
	z := base("zaentrum-beta")
	z.Spec.Hostname = "zaentrum.beta.example.org"
	z.Spec.Identity.Mode = "external"
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	z.Spec.Network.IssuerHostAliasIP = "192.0.2.10"
	z.Spec.ImagePullSecrets = []string{"registry-pull"}
	z.Spec.PartOf = "zaentrum-beta"
	z.Spec.EventStreaming.Mode = "external"
	z.Spec.EventStreaming.Bootstrap = "broker.events.svc:9093"
	z.Spec.EventStreaming.CertSecret = "kafka-mtls"
	z.Spec.EventStreaming.TopicPrefix = "zaentrum-beta."

	vals, err := AddonPlatformValues(z)
	require.NoError(t, err)
	assert.Equal(t, "zaentrum-beta", vals["namespace"])
	assert.Equal(t, "zaentrum.beta.example.org", vals["hostname"])
	assert.Equal(t, "https://sso.example.org/realms/x", vals["issuer"])
	assert.Equal(t, "192.0.2.10", vals["issuerHostAliasIP"])
	assert.Equal(t, []interface{}{"registry-pull"}, vals["imagePullSecrets"])
	assert.Equal(t, "zaentrum-beta-addons", vals["partOf"])
	assert.Equal(t, map[string]interface{}{
		"brokers":     "broker.events.svc:9093",
		"topicPrefix": "zaentrum-beta.",
		"tlsSecret":   "kafka-mtls",
	}, vals["events"])

	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	assert.Equal(t, vals["events"].(map[string]interface{})["brokers"], envValue(t, objs, "portal-api", "KAFKA_BROKERS"))
	assert.Equal(t, vals["events"].(map[string]interface{})["topicPrefix"], envValue(t, objs, "portal-api", "KAFKA_TOPIC_PREFIX"))
}

// A cert secret on the bundled broker is ignored by the platform (plaintext),
// so addons must not be told to use TLS either.
func TestAddonPlatformValuesBundledIgnoresCertSecret(t *testing.T) {
	z := base("zaentrum")
	z.Spec.EventStreaming.CertSecret = "kafka-mtls"
	vals, err := AddonPlatformValues(z)
	require.NoError(t, err)
	assert.Equal(t, "", vals["events"].(map[string]interface{})["tlsSecret"])
}

// External mode without a bootstrap fails the platform render; the addon
// values surface the same error instead of inventing a broker.
func TestAddonPlatformValuesExternalNeedsBootstrap(t *testing.T) {
	z := base("zaentrum")
	z.Spec.EventStreaming.Mode = "external"
	_, err := AddonPlatformValues(z)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eventStreaming.bootstrap is required")
}
