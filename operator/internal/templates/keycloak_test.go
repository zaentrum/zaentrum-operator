package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const keycloakHeap = "-XX:MaxRAMPercentage=50 -XX:MinRAMPercentage=50 -XX:InitialRAMPercentage=30"

// Keycloak's JVM takes at most half of its container's memory limit for the
// heap, and starts on less than a third, so what is not heap - about 300 to
// 350 MiB at the first start - fits beside it within the limit.
func TestKeycloakHeapShareLeavesRoomBesideTheHeap(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))

	assert.Equal(t, keycloakHeap, envValue(t, objs, "keycloak", "JAVA_OPTS_KC_HEAP"))

	// The limit the shares are of.
	d := find(t, objs, "Deployment", "keycloak")
	require.NotNil(t, d)
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NotEmpty(t, containers)
	limit, _, _ := unstructured.NestedString(containers[0].(map[string]interface{}), "resources", "limits", "memory")
	assert.Equal(t, "1Gi", limit)
}

// The share is the Keycloak server's alone: the realm Job and the chart test
// run the admin CLI from the same image, and nothing sizes their JVMs by it.
func TestKeycloakHeapShareOnlyOnTheServer(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))
	for _, name := range []string{"zaentrum-realm", "zaentrum-verify"} {
		j := find(t, objs, "Job", name)
		require.NotNil(t, j, "Job %s", name)
		for _, field := range []string{"initContainers", "containers"} {
			cs, _, _ := unstructured.NestedSlice(j.Object, "spec", "template", "spec", field)
			for _, c := range cs {
				env, _, _ := unstructured.NestedSlice(c.(map[string]interface{}), "env")
				for _, e := range env {
					assert.NotEqual(t, "JAVA_OPTS_KC_HEAP", e.(map[string]interface{})["name"],
						"Job %s, %s %v", name, field, c.(map[string]interface{})["name"])
				}
			}
		}
	}
}
