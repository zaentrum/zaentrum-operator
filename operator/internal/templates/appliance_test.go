package templates

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The Zaentrum the appliance boots (deploy/allinone/manifests/20-zaentrum.yaml,
// a copy of config/samples) makes a fresh box's titles playable: the media
// pipeline runs, on the CPU, and nothing waits for a GPU the box has not got.
// What its pods ask for fits a machine of four cores and 8 GiB beside k3s,
// Traefik, CoreDNS and the platform's own Jobs, which keep a core and 2 GiB:
// the scheduler places nothing whose requests do not fit, and the platform
// would never be Ready.
func TestTheAppliancesZaentrumFitsAPlainBox(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/allinone/manifests/20-zaentrum.yaml")
	require.NoError(t, err)
	var z zaentrumv1alpha1.Zaentrum
	require.NoError(t, yaml.UnmarshalStrict(raw, &z))
	sample, err := os.ReadFile("../../config/samples/zaentrum_v1alpha1_zaentrum.yaml")
	require.NoError(t, err)
	assert.Equal(t, string(sample), string(raw), "the appliance boots the sample (build.sh render)")
	// The API server's defaults for what the file leaves out.
	if z.Spec.Identity.Mode == "" {
		z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityBundled
	}

	objs, err := Render(NewValues(&z))
	require.NoError(t, err)
	for _, name := range []string{"analyzer", "transcoder", "packager", "katalog-ingest"} {
		require.NotNil(t, find(t, objs, "Deployment", name), "the pipeline runs: %s", name)
	}
	assert.NotContains(t, fmt.Sprintf("%v", objs), "nvidia.com/gpu", "nothing asks for, tolerates or looks for a GPU")
	assert.Equal(t, int64(1), replicas(t, objs, "packager"))

	cpu, memory := resource.Quantity{}, resource.Quantity{}
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		var dep appsv1.Deployment
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &dep))
		n := int64(1)
		if dep.Spec.Replicas != nil {
			n = int64(*dep.Spec.Replicas)
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			for i := int64(0); i < n; i++ {
				cpu.Add(*c.Resources.Requests.Cpu())
				memory.Add(*c.Resources.Requests.Memory())
			}
		}
	}
	assert.LessOrEqual(t, cpu.MilliValue(), int64(3000), "the platform asks for %s CPUs", cpu.String())
	assert.LessOrEqual(t, memory.Value(), int64(6<<30), "the platform asks for %s of memory", memory.String())
	t.Logf("the appliance's platform asks for %s CPUs and %s of memory", cpu.String(), memory.String())
}
