package templates

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const keycloakHeap = "-XX:MaxRAM=1g -XX:MaxRAMPercentage=50 -XX:MinRAMPercentage=50 -XX:InitialRAMPercentage=30"

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

// The shares are of MaxRAM, which the chart names because the Java runtime
// does not always take it from the container's limit: the two are the same
// amount, so a change of one without the other fails here.
func TestKeycloakHeapIsSizedFromItsOwnLimit(t *testing.T) {
	objs := renderCR(t, base("zaentrum"))
	opts := envValue(t, objs, "keycloak", "JAVA_OPTS_KC_HEAP")
	var maxRAM string
	for _, f := range strings.Fields(opts) {
		if v, ok := strings.CutPrefix(f, "-XX:MaxRAM="); ok {
			maxRAM = v
		}
	}
	require.NotEmpty(t, maxRAM, "JAVA_OPTS_KC_HEAP names MaxRAM")
	ram, err := jvmBytes(maxRAM)
	require.NoError(t, err)

	d := find(t, objs, "Deployment", "keycloak")
	require.NotNil(t, d)
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NotEmpty(t, containers)
	limit, _, _ := unstructured.NestedString(containers[0].(map[string]interface{}), "resources", "limits", "memory")
	q, err := resource.ParseQuantity(limit)
	require.NoError(t, err)

	assert.Equal(t, q.Value(), ram, "MaxRAM %s and the memory limit %s are the same amount", maxRAM, limit)
}

// jvmBytes is the bytes of a JVM size option's value: a number with k, m, g
// or t (either case) for KiB, MiB, GiB or TiB, or none for bytes.
func jvmBytes(v string) (int64, error) {
	mult := int64(1)
	if n := len(v); n > 0 {
		switch v[n-1] {
		case 'k', 'K':
			mult = 1 << 10
		case 'm', 'M':
			mult = 1 << 20
		case 'g', 'G':
			mult = 1 << 30
		case 't', 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			v = v[:n-1]
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n * mult, err
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
