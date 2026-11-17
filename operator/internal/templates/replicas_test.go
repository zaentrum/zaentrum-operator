package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A part set to 0 replicas stops: the count is taken as it is, 0 among them,
// through the resource's spec.replicas and through the chart's values alike.
// A part not named keeps its default, and a count left null keeps it too.
func TestAPartSetToZeroReplicasStops(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Replicas = map[string]int32{"chino-web": 0, "chino-stream": 3}
	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	assert.Equal(t, int64(0), replicas(t, objs, "chino-web"), "set to 0 in spec.replicas: stopped")
	assert.Equal(t, int64(3), replicas(t, objs, "chino-stream"), "set to 3: three")
	assert.Equal(t, int64(1), replicas(t, objs, "chino-api"), "not named: its default")

	for name, v := range map[string]interface{}{"0": 0, "null": nil} {
		objs := helmRender(t, map[string]interface{}{"services": map[string]interface{}{
			"chino-web": map[string]interface{}{"replicas": v},
		}})
		want := int64(0)
		if v == nil {
			want = 1
		}
		assert.Equal(t, want, replicas(t, objs, "chino-web"), "helm values, replicas %s", name)
	}
}
