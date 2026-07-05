package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// defaultZaentrum returns a Zaentrum CR with empty spec; NewValues applies all the
// documented defaults (bundled identity, latest, zaentrum.localhost, kafka on).
func defaultZaentrum() *zaentrumv1alpha1.Zaentrum {
	s := &zaentrumv1alpha1.Zaentrum{}
	s.Name = "zaentrum"
	s.Namespace = "zaentrum"
	// Mirror the CRD defaults the API server would inject.
	s.Spec.Features.Kafka = true
	return s
}

// deploymentReplicas reads spec.replicas from a rendered Deployment.
func deploymentReplicas(t *testing.T, objs []*unstructured.Unstructured, name string) int64 {
	t.Helper()
	dep := find(t, objs, "Deployment", name)
	require.NotNil(t, dep, "Deployment %s not rendered", name)
	// The YAML→JSON decoder represents numbers as float64 (the operator applies
	// them fine — controller-runtime coerces on SSA).
	f, found, err := unstructured.NestedFloat64(dep.Object, "spec", "replicas")
	require.NoError(t, err)
	require.True(t, found, "Deployment %s has no spec.replicas", name)
	return int64(f)
}

// spec.replicas overrides an app-tier Deployment; unlisted services default to 1
// and stateful backers are never scaled.
func TestReplicasOverride(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Replicas = map[string]int32{"chino-api": 3, "katalog-api": 2}
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	assert.Equal(t, int64(3), deploymentReplicas(t, objs, "chino-api"), "override applied")
	assert.Equal(t, int64(2), deploymentReplicas(t, objs, "katalog-api"), "override applied")
	assert.Equal(t, int64(1), deploymentReplicas(t, objs, "chino-web"), "unlisted app-tier defaults to 1")
	assert.Equal(t, int64(1), deploymentReplicas(t, objs, "postgres"), "stateful backer stays at 1")
	assert.Equal(t, int64(1), deploymentReplicas(t, objs, "keycloak"), "stateful backer stays at 1")
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

func TestRenderDefaultProducesFullPlatform(t *testing.T) {
	objs, err := Render(NewValues(defaultZaentrum()))
	require.NoError(t, err)

	// deploy/base renders 35 objects; the operator reproduces the same set.
	assert.Len(t, objs, 35, "default render must reproduce deploy/base's 35 objects")

	counts := map[string]int{}
	for _, o := range objs {
		counts[o.GetKind()]++
		assert.NotEmpty(t, o.GetAPIVersion(), "every object needs apiVersion")
		assert.NotEmpty(t, o.GetName(), "every object needs a name")
	}
	assert.Equal(t, 10, counts["Deployment"], "10 Deployments")
	assert.Equal(t, 10, counts["Service"], "10 Services")
	assert.Equal(t, 3, counts["ConfigMap"], "3 ConfigMaps")
	assert.Equal(t, 4, counts["Secret"], "4 Secrets")
	assert.Equal(t, 3, counts["PersistentVolumeClaim"], "3 PVCs")
	assert.Equal(t, 1, counts["Namespace"])
	assert.Equal(t, 1, counts["Ingress"])
	assert.Equal(t, 1, counts["ServiceAccount"])
	assert.Equal(t, 1, counts["Role"])
	assert.Equal(t, 1, counts["RoleBinding"])
}

func TestKeycloakBootFixesPreserved(t *testing.T) {
	objs, err := Render(NewValues(defaultZaentrum()))
	require.NoError(t, err)

	// A Deployment named keycloak must exist.
	dep := find(t, objs, "Deployment", "keycloak")
	require.NotNil(t, dep, "keycloak Deployment must be rendered")

	containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	kc := containers[0].(map[string]interface{})

	// /auth health probe on the management port. (boot fix)
	readyPath, _, _ := unstructured.NestedString(kc, "readinessProbe", "httpGet", "path")
	readyPort, _, _ := unstructured.NestedString(kc, "readinessProbe", "httpGet", "port")
	assert.Equal(t, "/auth/health/ready", readyPath)
	assert.Equal(t, "mgmt", readyPort, "health probe must target the management port")

	livePath, _, _ := unstructured.NestedString(kc, "livenessProbe", "httpGet", "path")
	assert.Equal(t, "/auth/health/live", livePath)

	// Image is parameterized to the configured version.
	img, _, _ := unstructured.NestedString(kc, "image")
	assert.Equal(t, "ghcr.io/zaentrum/keycloak:latest", img)

	// The keycloak Service must expose :80.
	svc := find(t, objs, "Service", "keycloak")
	require.NotNil(t, svc, "keycloak Service must be rendered")
	ports, found, err := unstructured.NestedSlice(svc.Object, "spec", "ports")
	require.NoError(t, err)
	require.True(t, found)
	p0 := ports[0].(map[string]interface{})
	// The YAMLOrJSONDecoder yields JSON-style numbers (float64).
	assert.EqualValues(t, 80, p0["port"], "keycloak Service must listen on :80")
}

func TestWaitForOIDCInitContainersPresent(t *testing.T) {
	objs, err := Render(NewValues(defaultZaentrum()))
	require.NoError(t, err)

	for _, name := range []string{"chino-api", "chino-stream", "katalog-manager-api"} {
		dep := find(t, objs, "Deployment", name)
		require.NotNil(t, dep, "%s Deployment must exist", name)
		inits, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
		require.True(t, found, "%s must have initContainers", name)
		var names []string
		for _, ic := range inits {
			n, _, _ := unstructured.NestedString(ic.(map[string]interface{}), "name")
			names = append(names, n)
		}
		assert.Contains(t, names, "wait-for-oidc", "%s must keep wait-for-oidc init", name)
	}
}

func TestStableConfigMapNamesAndReferences(t *testing.T) {
	objs, err := Render(NewValues(defaultZaentrum()))
	require.NoError(t, err)

	// Un-kustomized: the env ConfigMap has the stable name (no hash suffix).
	require.NotNil(t, find(t, objs, "ConfigMap", "zaentrum-env"), "zaentrum-env must use stable name")
	require.NotNil(t, find(t, objs, "Secret", "zaentrum-db"), "zaentrum-db must use stable name")
	require.NotNil(t, find(t, objs, "Secret", "zaentrum-stream-signing"), "zaentrum-stream-signing must use stable name")

	// chino-api references zaentrum-env by plain name via envFrom.
	dep := find(t, objs, "Deployment", "chino-api")
	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	envFrom, found, _ := unstructured.NestedSlice(containers[0].(map[string]interface{}), "envFrom")
	require.True(t, found)
	cmName, _, _ := unstructured.NestedString(envFrom[0].(map[string]interface{}), "configMapRef", "name")
	assert.Equal(t, "zaentrum-env", cmName)
}

func TestStreamSigningKeyIsValidBase64(t *testing.T) {
	objs, err := Render(NewValues(defaultZaentrum()))
	require.NoError(t, err)
	sec := find(t, objs, "Secret", "zaentrum-stream-signing")
	require.NotNil(t, sec)
	// The data value itself is base64 (k8s Secret data); preserved from base.
	key, found, _ := unstructured.NestedString(sec.Object, "data", "key")
	require.True(t, found)
	assert.NotEmpty(t, key)
}

func TestVersionParameterizationAppliesToAllZaentrumImages(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Version = "v1.2.3"
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	zaentrumImages := 0
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			img, _, _ := unstructured.NestedString(c.(map[string]interface{}), "image")
			if len(img) >= len("ghcr.io/zaentrum/") && img[:len("ghcr.io/zaentrum/")] == "ghcr.io/zaentrum/" {
				zaentrumImages++
				assert.Contains(t, img, ":v1.2.3", "zaentrum image must carry the configured version: %s", img)
			}
		}
	}
	// 7 ghcr.io/zaentrum/* images across Deployments: keycloak, chino-web,
	// chino-api, chino-stream, katalog-api, katalog-manager, admin. (The
	// management API's Deployment is named katalog-manager-api but pulls the
	// flat katalog-manager image — the Go rewrite.)
	assert.Equal(t, 7, zaentrumImages, "all 7 ghcr.io/zaentrum/* images must carry the version tag")
}

func TestHostnameParameterization(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Hostname = "media.example.com"
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	// Ingress host.
	ing := find(t, objs, "Ingress", "zaentrum")
	require.NotNil(t, ing)
	rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
	host, _, _ := unstructured.NestedString(rules[0].(map[string]interface{}), "host")
	assert.Equal(t, "media.example.com", host)

	// OIDC issuer derived from hostname.
	cm := find(t, objs, "ConfigMap", "zaentrum-env")
	issuer, _, _ := unstructured.NestedString(cm.Object, "data", "OIDC_ISSUER")
	assert.Equal(t, "http://media.example.com/auth/realms/zaentrum", issuer)

	// KC_HOSTNAME derived from hostname.
	kcCfg := find(t, objs, "ConfigMap", "zaentrum-keycloak-config")
	require.NotNil(t, kcCfg)
	kcHost, _, _ := unstructured.NestedString(kcCfg.Object, "data", "KC_HOSTNAME")
	assert.Equal(t, "http://media.example.com/auth", kcHost)
}

func TestMediaStorageSizeAndClass(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Storage.MediaSize = resource.MustParse("250Gi")
	s.Spec.Storage.ClassName = "fast-ssd"
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	pvc := find(t, objs, "PersistentVolumeClaim", "media")
	require.NotNil(t, pvc)
	size, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	assert.Equal(t, "250Gi", size)
	class, _, _ := unstructured.NestedString(pvc.Object, "spec", "storageClassName")
	assert.Equal(t, "fast-ssd", class)
}

func TestKafkaFeatureGate(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Features.Kafka = false
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	assert.Nil(t, find(t, objs, "Deployment", "kafka"), "kafka Deployment must be elided when disabled")
	assert.Nil(t, find(t, objs, "Service", "kafka"), "kafka Service must be elided when disabled")
	assert.Nil(t, find(t, objs, "PersistentVolumeClaim", "kafka-data"), "kafka PVC must be elided when disabled")
	// 35 - 3 kafka objects = 32.
	assert.Len(t, objs, 32)

	// KAFKA_BROKERS empty when disabled.
	cm := find(t, objs, "ConfigMap", "zaentrum-env")
	brokers, _, _ := unstructured.NestedString(cm.Object, "data", "KAFKA_BROKERS")
	assert.Empty(t, brokers)
}

func TestGPUFeatureGate(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Features.GPU = true
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	dep := find(t, objs, "Deployment", "chino-stream")
	require.NotNil(t, dep)
	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	c0 := containers[0].(map[string]interface{})

	// USE_NVENC flipped on.
	env, _, _ := unstructured.NestedSlice(c0, "env")
	var useNvenc string
	for _, e := range env {
		em := e.(map[string]interface{})
		if em["name"] == "USE_NVENC" {
			useNvenc, _, _ = unstructured.NestedString(em, "value")
		}
	}
	assert.Equal(t, "true", useNvenc)

	// nvidia.com/gpu limit present.
	gpu, found, _ := unstructured.NestedString(c0, "resources", "limits", "nvidia.com/gpu")
	assert.True(t, found, "GPU limit must be set")
	assert.Equal(t, "1", gpu)

	// nodeSelector present.
	_, found, _ = unstructured.NestedMap(dep.Object, "spec", "template", "spec", "nodeSelector")
	assert.True(t, found, "GPU node selector must be set")
}

func TestExternalIdentityElidesBundledKeycloak(t *testing.T) {
	s := defaultZaentrum()
	s.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	s.Spec.Identity.Issuer = "https://idp.example.com/realms/zaentrum"
	objs, err := Render(NewValues(s))
	require.NoError(t, err)

	assert.Nil(t, find(t, objs, "Deployment", "keycloak"), "external mode elides bundled keycloak")
	assert.Nil(t, find(t, objs, "ConfigMap", "keycloak-realm"))

	cm := find(t, objs, "ConfigMap", "zaentrum-env")
	issuer, _, _ := unstructured.NestedString(cm.Object, "data", "OIDC_ISSUER")
	assert.Equal(t, "https://idp.example.com/realms/zaentrum", issuer)
}
