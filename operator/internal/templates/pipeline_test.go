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
// spec.pipeline existed (testdata/pipeline-gpu.yaml), so not one of them rolls —
// but for EXTRAS_GROUP_ID, which the transcoder and the packager gained with
// spec.eventStreaming.groupPrefix: it names the extras' groups their own
// defaults named, so each rolls once and consumes where it did. "gpu" said out
// loud renders the same.
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
// its ladder, its extras' ladder and keyframe interval, the packager its
// segments, surround audio, HLS subtitles and language order — and nothing set
// leaves every worker its own default.
func TestPipelineSettingsReachTheWorkers(t *testing.T) {
	settings := []string{"LADDER", "EXTRA_LADDER", "SEGMENT_SECONDS", "SURROUND_AUDIO", "HLS_SUBTITLES", "PREFERRED_LANGUAGES"}
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
		z.Spec.Pipeline.ExtraLadder = "source:hevc"
		z.Spec.Pipeline.SegmentSeconds = 4
		z.Spec.Pipeline.SurroundAudio = "eac3"
		z.Spec.Pipeline.HLSSubtitles = true
		z.Spec.Pipeline.PreferredLanguages = []string{"de", "en"}
		objs = renderCR(t, z)
		tc := envByName(typedDeployment(t, objs, "transcoder").Spec.Template.Spec.Containers[0])
		assert.Equal(t, "source,720p", tc["LADDER"].Value, encoder)
		assert.Equal(t, "source:hevc", tc["EXTRA_LADDER"].Value, encoder)
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
		assert.NotContains(t, pk, "EXTRA_LADDER", encoder)
	}

	// As helm renders it: values.yaml's empty extraLadder passes nothing on,
	// and one set reaches the transcoder.
	tc := envByName(typedDeployment(t, helmRender(t, map[string]interface{}{
		"features": map[string]interface{}{"pipeline": true},
	}), "transcoder").Spec.Template.Spec.Containers[0])
	assert.NotContains(t, tc, "EXTRA_LADDER", "helm: the transcoder's own default")
	tc = envByName(typedDeployment(t, helmRender(t, map[string]interface{}{
		"features": map[string]interface{}{"pipeline": true},
		"pipeline": map[string]interface{}{"extraLadder": "source:hevc"},
	}), "transcoder").Spec.Template.Spec.Containers[0])
	assert.Equal(t, "source:hevc", tc["EXTRA_LADDER"].Value, "helm")

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
	z.Spec.Pipeline.ExtraLadder = "source:hevc"
	objs := renderCR(t, z)
	assert.Nil(t, find(t, objs, "Deployment", "transcoder"))
	assert.False(t, strings.Contains(fmt.Sprintf("%v", objs), "LADDER"))
}

// groupEnv is every environment variable of a render's Deployments named for a
// consumer group, "<deployment> <name>" to its value.
func groupEnv(objs []*unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		for _, key := range []string{"initContainers", "containers"} {
			containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", key)
			for _, c := range containers {
				env, _, _ := unstructured.NestedSlice(c.(map[string]interface{}), "env")
				for _, e := range env {
					m := e.(map[string]interface{})
					if name, _ := m["name"].(string); strings.Contains(name, "GROUP") {
						value, _ := m["value"].(string)
						out[o.GetName()+" "+name] = value
					}
				}
			}
		}
	}
	return out
}

// sharedBrokerCR is an instance with the pipeline on a broker it shares with
// others: a topic prefix and an mTLS user of its own, and groupPrefix.
func sharedBrokerCR(groupPrefix string) *zaentrumv1alpha1.Zaentrum {
	z := pipelineCR("cpu")
	z.Spec.EventStreaming.Mode = "external"
	z.Spec.EventStreaming.Bootstrap = "broker.events.svc:9093"
	z.Spec.EventStreaming.CertSecret = "kafka-mtls"
	z.Spec.EventStreaming.TopicPrefix = "tenant."
	z.Spec.EventStreaming.GroupPrefix = groupPrefix
	return z
}

// Instances that share a broker never share a consumer group: every group the
// pipeline's workers and the catalog enricher consume in carries
// spec.eventStreaming.groupPrefix. A group two instances shared would be led by
// one of them, which cannot see the other's topics, and the other's workers
// would be given no partition and process nothing. Without a prefix each group
// keeps exactly the name it had, and with it its committed offsets — through
// the operator and through helm, bundled or on a shared broker. What the
// prefix does not name stays as it was: katalog-ingest's group, a legacy
// scheduler's on other topics, and katalog-manager-api's KAFKA_GROUP_ID, which
// katalog-manager does not read. Nor does the prefix reach a topic.
func TestGroupPrefixNamesTheInstancesConsumerGroups(t *testing.T) {
	consumers := []struct{ deployment, env, unprefixed, prefixed string }{
		{"analyzer", "KAFKA_GROUP_ID", "analyzer-workers", "tenant-analyzer-workers"},
		{"transcoder", "KAFKA_GROUP_ID", "transcoder-workers", "tenant-transcoder-workers"},
		{"transcoder", "EXTRAS_GROUP_ID", "transcoder-extras", "tenant-transcoder-extras"},
		{"packager", "KAFKA_GROUP_ID", "packager-workers", "tenant-packager-workers"},
		{"packager", "EXTRAS_GROUP_ID", "packager-extras", "tenant-packager-extras"},
		{"katalog-manager-api", "KAFKA_ENRICHER_GROUP", "katalog-enricher", "tenant-katalog-enricher"},
	}
	helmPrefixed := helmRender(t, map[string]interface{}{
		"features":       map[string]interface{}{"pipeline": true},
		"eventStreaming": map[string]interface{}{"groupPrefix": "tenant-"},
	})
	for name, tc := range map[string]struct {
		objs     []*unstructured.Unstructured
		prefixed bool
	}{
		"operator, bundled":           {renderCR(t, pipelineCR("gpu")), false},
		"operator, shared, no prefix": {renderCR(t, sharedBrokerCR("")), false},
		"operator, shared, tenant-":   {renderCR(t, sharedBrokerCR("tenant-")), true},
		"helm, values.yaml":           {helmRender(t, map[string]interface{}{"features": map[string]interface{}{"pipeline": true}}), false},
		"helm, groupPrefix tenant-":   {helmPrefixed, true},
	} {
		want := map[string]string{
			"katalog-ingest KAFKA_GROUP_ID":      "zaentrum-demo",
			"katalog-manager-api KAFKA_GROUP_ID": "zaentrum-demo",
		}
		for _, c := range consumers {
			want[c.deployment+" "+c.env] = c.unprefixed
			if tc.prefixed {
				want[c.deployment+" "+c.env] = c.prefixed
			}
		}
		assert.Equal(t, want, groupEnv(tc.objs), name)
	}

	objs := renderCR(t, sharedBrokerCR("tenant-"))
	assert.Equal(t, "tenant.catalog.item.enriched", envValue(t, objs, "analyzer", "CONSUME_TOPIC"))
	assert.Equal(t, "tenant.", envValue(t, objs, "katalog-manager-api", "KAFKA_TOPIC_PREFIX"))
	assert.Equal(t, "stube.catalog.item.analyzed", envValue(t, helmPrefixed, "transcoder", "CONSUME_TOPIC"))
}
