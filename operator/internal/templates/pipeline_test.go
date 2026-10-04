package templates

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// typedDeployment is the rendered Deployment of that name, typed.
func typedDeployment(t *testing.T, objs []*unstructured.Unstructured, name string) *appsv1.Deployment {
	t.Helper()
	u := find(t, objs, "Deployment", name)
	require.NotNil(t, u, "no Deployment %s", name)
	var dep appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dep))
	return &dep
}

// pipelineCR is the self-host profile with the media pipeline on.
func pipelineCR(encoder string) *zaentrumv1alpha1.Zaentrum {
	z := base("zaentrum")
	z.Spec.Features.Pipeline = true
	z.Spec.Pipeline.Encoder = encoder
	return z
}

// An install that runs the pipeline and says nothing of the encoder runs it as
// before: on the GPU, its workers byte for byte what the demo's were before
// spec.pipeline existed (testdata/pipeline-gpu.yaml), so not one of them rolls.
// "gpu" said out loud renders the same.
func TestPipelineOnTheGPURendersAsBefore(t *testing.T) {
	raw, err := os.ReadFile("testdata/pipeline-gpu.yaml")
	require.NoError(t, err)
	var want []map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &want))

	for _, encoder := range []string{"", "gpu"} {
		z := demoCR("zaentrum-demo")
		z.Spec.Pipeline.Encoder = encoder
		v := NewValues(z)
		v.OpenShift = true
		objs, err := Render(v)
		require.NoError(t, err)
		for i, name := range []string{"analyzer", "packager", "transcoder"} {
			got := find(t, objs, "Deployment", name)
			require.NotNil(t, got)
			assert.Equal(t, want[i], got.Object, "encoder %q: %s changed, and would roll", encoder, name)
		}
	}

	tc := typedDeployment(t, renderCR(t, pipelineCR("gpu")), "transcoder")
	pod := tc.Spec.Template.Spec
	assert.Equal(t, map[string]string{"nvidia.com/gpu.present": "true"}, pod.NodeSelector)
	gpu := pod.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	assert.Equal(t, "1", gpu.String())
	assert.NotContains(t, envByName(pod.Containers[0]), "ENCODER", "the transcoder's own auto, as before")
}

// On the CPU nothing in the pipeline asks for, tolerates or looks for a GPU:
// the transcoder schedules on any node, encodes with libx264/libx265, and asks
// for what fits a small box beside the rest of the platform.
func TestPipelineOnTheCPUAsksForNoGPU(t *testing.T) {
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator": renderCR(t, pipelineCR("cpu")),
		"helm": helmRender(t, map[string]interface{}{
			"features": map[string]interface{}{"pipeline": true},
			"pipeline": map[string]interface{}{"encoder": "cpu"},
		}),
	} {
		for _, dep := range []string{"analyzer", "packager", "transcoder", "katalog-ingest"} {
			d := typedDeployment(t, objs, dep)
			blob := fmt.Sprintf("%v", find(t, objs, "Deployment", dep).Object)
			assert.NotContains(t, blob, "nvidia.com/gpu", "%s: %s", name, dep)
			assert.Empty(t, d.Spec.Template.Spec.NodeSelector, "%s: %s", name, dep)
			assert.Empty(t, d.Spec.Template.Spec.Tolerations, "%s: %s", name, dep)
		}

		tc := typedDeployment(t, objs, "transcoder").Spec.Template.Spec.Containers[0]
		env := envByName(tc)
		assert.Equal(t, "cpu", env["ENCODER"].Value, name)
		for _, gone := range []string{"NVENC_PRESET", "NVENC_CQ", "NVENC_MAXRATE_1080P_MBPS", "NVENC_MAXRATE_2160P_MBPS", "LD_LIBRARY_PATH"} {
			assert.NotContains(t, env, gone, "%s: %s means nothing without NVENC", name, gone)
		}
		assert.Equal(t, "500m", tc.Resources.Requests.Cpu().String(), name)
		assert.Equal(t, "1Gi", tc.Resources.Requests.Memory().String(), name)
		assert.Equal(t, "4", tc.Resources.Limits.Cpu().String(), name)
		assert.Equal(t, "8Gi", tc.Resources.Limits.Memory().String(), name)
		assert.Len(t, tc.Resources.Requests, 2, "%s: cpu and memory, nothing else", name)
		assert.Len(t, tc.Resources.Limits, 2, "%s: cpu and memory, nothing else", name)
	}

	_, err := render(map[string]interface{}{
		"features": map[string]interface{}{"pipeline": true},
		"pipeline": map[string]interface{}{"encoder": "tpu"},
	}, "zaentrum", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline.encoder is gpu or cpu")
}

// What spec.pipeline sets reaches the workers that read it — the transcoder
// its ladder and keyframe interval, the packager its segments, surround audio,
// HLS subtitles and language order — and nothing set leaves every worker its
// own default.
func TestPipelineSettingsReachTheWorkers(t *testing.T) {
	settings := []string{"LADDER", "SEGMENT_SECONDS", "SURROUND_AUDIO", "HLS_SUBTITLES", "PREFERRED_LANGUAGES"}
	for _, encoder := range []string{"gpu", "cpu"} {
		objs := renderCR(t, pipelineCR(encoder))
		for _, dep := range []string{"transcoder", "packager"} {
			env := envByName(typedDeployment(t, objs, dep).Spec.Template.Spec.Containers[0])
			for _, s := range settings {
				assert.NotContains(t, env, s, "%s: %s sets %s by itself", encoder, dep, s)
			}
		}

		z := pipelineCR(encoder)
		z.Spec.Pipeline.Ladder = "source,720p"
		z.Spec.Pipeline.SegmentSeconds = 4
		z.Spec.Pipeline.SurroundAudio = "eac3"
		z.Spec.Pipeline.HLSSubtitles = true
		z.Spec.Pipeline.PreferredLanguages = []string{"de", "en"}
		objs = renderCR(t, z)
		tc := envByName(typedDeployment(t, objs, "transcoder").Spec.Template.Spec.Containers[0])
		assert.Equal(t, "source,720p", tc["LADDER"].Value, encoder)
		assert.Equal(t, "4", tc["SEGMENT_SECONDS"].Value, encoder)
		for _, s := range []string{"SURROUND_AUDIO", "HLS_SUBTITLES", "PREFERRED_LANGUAGES"} {
			assert.NotContains(t, tc, s, "%s: the packager's, not the transcoder's", encoder)
		}
		pk := envByName(typedDeployment(t, objs, "packager").Spec.Template.Spec.Containers[0])
		assert.Equal(t, "4", pk["SEGMENT_SECONDS"].Value, "%s: the packager cuts where the transcoder put keyframes", encoder)
		assert.Equal(t, "eac3", pk["SURROUND_AUDIO"].Value, encoder)
		assert.Equal(t, "true", pk["HLS_SUBTITLES"].Value, encoder)
		assert.Equal(t, "de,en", pk["PREFERRED_LANGUAGES"].Value, encoder)
		assert.NotContains(t, pk, "LADDER", encoder)
	}

	// "off" said out loud is passed on, so a later default of the packager's
	// cannot turn it on.
	z := pipelineCR("cpu")
	z.Spec.Pipeline.SurroundAudio = "off"
	pk := envByName(typedDeployment(t, renderCR(t, z), "packager").Spec.Template.Spec.Containers[0])
	assert.Equal(t, "off", pk["SURROUND_AUDIO"].Value)

	// Without the pipeline there is nothing to configure.
	z = base("zaentrum")
	z.Spec.Pipeline.Encoder = "cpu"
	z.Spec.Pipeline.Ladder = "source,720p"
	objs := renderCR(t, z)
	assert.Nil(t, find(t, objs, "Deployment", "transcoder"))
	assert.False(t, strings.Contains(fmt.Sprintf("%v", objs), "LADDER"))
}
