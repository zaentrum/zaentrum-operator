package templates

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
)

// liveWorkerCR is the self-host profile with the live worker on, in mode.
func liveWorkerCR(mode string) *zaentrumv1alpha1.Zaentrum {
	z := base("zaentrum")
	z.Spec.Version = "v1.2.3"
	z.Spec.LiveWorker.Enabled = true
	z.Spec.LiveWorker.Mode = mode
	return z
}

func int32p(n int32) *int32 { return &n }

// typedService is the rendered Service of that name, typed.
func typedService(t *testing.T, objs []*unstructured.Unstructured, name string) *corev1.Service {
	t.Helper()
	u := find(t, objs, "Service", name)
	require.NotNil(t, u, "no Service %s", name)
	var svc corev1.Service
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &svc))
	return &svc
}

// An install that says nothing of the live worker has none: no Deployment or
// Service of it, no Secret for it, and chino-stream as before, byte for byte —
// so it does not roll; the same with spec.liveWorker set but not enabled.
func TestLiveWorkerIsOffUnlessEnabled(t *testing.T) {
	plain := base("zaentrum")
	set := base("zaentrum")
	set.Spec.LiveWorker = zaentrumv1alpha1.LiveWorkerSpec{Mode: "on", Slots: zaentrumv1alpha1.LiveWorkerSlots{GPU: int32p(8)}}
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":            renderCR(t, plain),
		"operator, disabled":  renderCR(t, set),
		"helm, values.yaml":   helmRender(t, nil),
		"helm, the pipeline":  helmRender(t, map[string]interface{}{"features": map[string]interface{}{"pipeline": true}}),
		"helm, said disabled": helmRender(t, map[string]interface{}{"liveWorker": map[string]interface{}{"enabled": false, "mode": "on"}}),
	} {
		assert.Nil(t, find(t, objs, "Deployment", "live-worker"), name)
		assert.Nil(t, find(t, objs, "Service", "live-worker"), name)
		assert.Nil(t, find(t, objs, "Secret", "zaentrum-live-worker"), name)
		env := envByName(typedDeployment(t, objs, "chino-stream").Spec.Template.Spec.Containers[0])
		for _, v := range []string{"LIVE_WORKER_URL", "LIVE_WORKER_MODE", "LIVE_WORKER_TOKEN"} {
			assert.NotContains(t, env, v, "%s: chino-stream is told of a live worker there is not", name)
		}
	}
	assert.Equal(t, find(t, renderCR(t, plain), "Deployment", "chino-stream").Object,
		find(t, renderCR(t, set), "Deployment", "chino-stream").Object, "a disabled live worker changes chino-stream")
}

// Enabled, the live worker is a Deployment and a Service of its own: the
// platform's image at the platform's version (digest-pinned as the others
// are), on a GPU placed as the transcoder's is — its node selector, its
// toleration, nvidia.com/gpu: 1 — with NVIDIA_DRIVER_CAPABILITIES=all and the
// CUDA backend, the library mounted read-only where chino-stream mounts it
// and as its media root, the token from its Secret; chino-stream is told
// where it is and the mode, and sends the token.
func TestLiveWorkerOnTheGPU(t *testing.T) {
	for _, openShift := range []bool{false, true} {
		v := NewValues(liveWorkerCR("shadow"))
		v.OpenShift = openShift
		objs, err := Render(v)
		require.NoError(t, err)
		dep := typedDeployment(t, objs, "live-worker")
		pod := dep.Spec.Template.Spec
		require.Len(t, pod.Containers, 1)
		c := pod.Containers[0]

		assert.Equal(t, "ghcr.io/zaentrum/live-worker:v1.2.3", c.Image)
		assert.True(t, digest.ZaentrumImages(digest.Parse(c.Image)), "the operator pins its digest as it pins the platform's other images")
		assert.Equal(t, corev1.PullAlways, c.ImagePullPolicy)
		assert.Equal(t, "Recreate", string(dep.Spec.Strategy.Type), "never two pods holding encode sessions at once")
		assert.Equal(t, int32(1), *dep.Spec.Replicas)

		tc := typedDeployment(t, renderCR(t, pipelineCR("gpu")), "transcoder").Spec.Template.Spec
		assert.Equal(t, tc.NodeSelector, pod.NodeSelector, "the transcoder's node selector")
		assert.Equal(t, map[string]string{"nvidia.com/gpu.present": "true"}, pod.NodeSelector)
		assert.Equal(t, tc.Tolerations, pod.Tolerations, "the transcoder's toleration")
		gpu := resource.MustParse("1")
		assert.True(t, gpu.Equal(c.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]), "limits %v", c.Resources.Limits)
		assert.True(t, gpu.Equal(c.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]), "requests %v", c.Resources.Requests)
		tcRes := tc.Containers[0].Resources
		assert.Equal(t, tcRes, c.Resources, "the transcoder's resources on the GPU")

		env := envByName(c)
		for k, want := range map[string]string{
			"LIVE_WORKER_LISTEN": ":8090", "LIVE_WORKER_MEDIA_ROOTS": "/var/lib/katalog", "LIVE_WORKER_GPU_SLOTS": "4",
			"LIVE_WORKER_CPU_SLOTS": "2", "LIVE_WORKER_HW": "cuda", "NVIDIA_DRIVER_CAPABILITIES": "all",
		} {
			assert.Equal(t, want, env[k].Value, k)
		}
		assert.True(t, strings.HasPrefix(env["LD_LIBRARY_PATH"].Value, "/opt/ffmpeg/lib:"), "the image's FFmpeg libraries first: %q", env["LD_LIBRARY_PATH"].Value)
		assert.Contains(t, env["LD_LIBRARY_PATH"].Value, "/usr/local/nvidia/lib64", "libcuda where the device plugin mounts it")
		secretRef(t, env, "LIVE_WORKER_TOKEN", "zaentrum-live-worker", "token")

		require.Len(t, c.Ports, 1)
		assert.Equal(t, int32(8090), c.Ports[0].ContainerPort)
		assert.Equal(t, "/readyz", c.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)

		// The library as chino-stream has it.
		cs := typedDeployment(t, objs, "chino-stream").Spec.Template.Spec
		assert.Equal(t, cs.Containers[0].VolumeMounts[0], c.VolumeMounts[0], "the media mount")
		assert.True(t, c.VolumeMounts[0].ReadOnly)
		assert.Equal(t, "/var/lib/katalog", c.VolumeMounts[0].MountPath)
		require.Len(t, pod.Volumes, 1)
		assert.Equal(t, cs.Volumes[0], pod.Volumes[0], "the media claim, read-only")
		assert.Equal(t, envByName(cs.Containers[0])["MEDIA_ROOT"].Value, env["LIVE_WORKER_MEDIA_ROOTS"].Value,
			"the paths chino-stream sends are under the worker's media root")

		sc := pod.SecurityContext
		require.NotNil(t, sc)
		assert.True(t, *sc.RunAsNonRoot)
		if openShift {
			assert.Nil(t, sc.RunAsUser, "the SCC gives the pod its user")
		} else {
			assert.Equal(t, int64(65532), *sc.RunAsUser)
		}

		svc := typedService(t, objs, "live-worker")
		assert.Equal(t, map[string]string{"app": "live-worker"}, svc.Spec.Selector)
		require.Len(t, svc.Spec.Ports, 1)
		assert.Equal(t, int32(80), svc.Spec.Ports[0].Port)
		assert.Equal(t, intstr.FromString("http"), svc.Spec.Ports[0].TargetPort)

		csEnv := envByName(cs.Containers[0])
		assert.Equal(t, "http://live-worker", csEnv["LIVE_WORKER_URL"].Value, "the Service, port 80")
		assert.Equal(t, "shadow", csEnv["LIVE_WORKER_MODE"].Value)
		secretRef(t, csEnv, "LIVE_WORKER_TOKEN", "zaentrum-live-worker", "token")
		assert.Nil(t, find(t, objs, "Secret", "zaentrum-live-worker"), "the operator makes the token itself")
	}
}

// What spec.liveWorker says reaches the worker and chino-stream: the mode,
// the slots, resources (a GPU added where they name none, the same in
// requests and limits), a node selector and tolerations of the CR's own in
// place of the transcoder's.
func TestLiveWorkerSettingsReachTheWorker(t *testing.T) {
	for _, mode := range []string{"off", "shadow", "on"} {
		objs := renderCR(t, liveWorkerCR(mode))
		assert.Equal(t, mode, envValue(t, objs, "chino-stream", "LIVE_WORKER_MODE"))
	}
	z := liveWorkerCR("")
	objs := renderCR(t, z)
	assert.Equal(t, "off", envValue(t, objs, "chino-stream", "LIVE_WORKER_MODE"), "unset: off, the worker running without traffic")

	z = liveWorkerCR("on")
	z.Spec.LiveWorker.Slots = zaentrumv1alpha1.LiveWorkerSlots{GPU: int32p(6), CPU: int32p(3)}
	z.Spec.LiveWorker.Resources = &corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("16Gi")},
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	z.Spec.LiveWorker.GPU.NodeSelector = map[string]string{"gpu.example.org/model": "a2000"}
	z.Spec.LiveWorker.GPU.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "live", Effect: corev1.TaintEffectNoExecute}}
	pod := typedDeployment(t, renderCR(t, z), "live-worker").Spec.Template.Spec
	c := pod.Containers[0]
	env := envByName(c)
	assert.Equal(t, "6", env["LIVE_WORKER_GPU_SLOTS"].Value)
	assert.Equal(t, "3", env["LIVE_WORKER_CPU_SLOTS"].Value)
	want := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("16Gi"),
			"nvidia.com/gpu": resource.MustParse("1")},
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu": resource.MustParse("1")},
	}
	assert.Equal(t, want, c.Resources)
	assert.Equal(t, map[string]string{"gpu.example.org/model": "a2000"}, pod.NodeSelector, "the CR's, not merged with the transcoder's")
	assert.Equal(t, z.Spec.LiveWorker.GPU.Tolerations, pod.Tolerations)

	// Two GPUs named in the limits are asked for in the requests too.
	z.Spec.LiveWorker.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}}
	c = typedDeployment(t, renderCR(t, z), "live-worker").Spec.Template.Spec.Containers[0]
	two := resource.MustParse("2")
	assert.True(t, two.Equal(c.Resources.Requests["nvidia.com/gpu"]), "requests %v", c.Resources.Requests)
	assert.True(t, two.Equal(c.Resources.Limits["nvidia.com/gpu"]), "limits %v", c.Resources.Limits)

	// As helm renders it.
	pod = typedDeployment(t, helmRender(t, map[string]interface{}{
		"liveWorker": map[string]interface{}{"enabled": true, "mode": "on", "slots": map[string]interface{}{"gpu": 2}},
	}), "live-worker").Spec.Template.Spec
	env = envByName(pod.Containers[0])
	assert.Equal(t, "2", env["LIVE_WORKER_GPU_SLOTS"].Value)
	assert.Equal(t, "2", env["LIVE_WORKER_CPU_SLOTS"].Value, "values.yaml's")
	assert.Equal(t, map[string]string{"nvidia.com/gpu.present": "true"}, pod.NodeSelector)
}

// Without the GPU nothing of the live worker asks for, tolerates or looks for
// one: no GPU slots, no CUDA, video on the CPU slots, and resources that fit
// beside the rest of the platform. A worker with no slot at all is refused.
func TestLiveWorkerOnTheCPUAsksForNoGPU(t *testing.T) {
	no := false
	z := liveWorkerCR("on")
	z.Spec.LiveWorker.GPU.Enabled = &no
	z.Spec.LiveWorker.Slots.GPU = int32p(6)
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator": renderCR(t, z),
		"helm": helmRender(t, map[string]interface{}{
			"liveWorker": map[string]interface{}{"enabled": true, "mode": "on", "gpu": map[string]interface{}{"enabled": false}},
		}),
	} {
		dep := find(t, objs, "Deployment", "live-worker")
		require.NotNil(t, dep, name)
		assert.NotContains(t, fmt.Sprintf("%v", dep.Object), "nvidia", name)
		pod := typedDeployment(t, objs, "live-worker").Spec.Template.Spec
		assert.Empty(t, pod.NodeSelector, name)
		assert.Empty(t, pod.Tolerations, name)
		c := pod.Containers[0]
		env := envByName(c)
		assert.Equal(t, "0", env["LIVE_WORKER_GPU_SLOTS"].Value, name)
		assert.Equal(t, "none", env["LIVE_WORKER_HW"].Value, name)
		assert.NotContains(t, env, "NVIDIA_DRIVER_CAPABILITIES", name)
		assert.NotContains(t, env, "LD_LIBRARY_PATH", name)
		assert.Equal(t, "500m", c.Resources.Requests.Cpu().String(), name)
		assert.Equal(t, "1Gi", c.Resources.Requests.Memory().String(), name)
		assert.Equal(t, "4", c.Resources.Limits.Cpu().String(), name)
		assert.Equal(t, "8Gi", c.Resources.Limits.Memory().String(), name)
		assert.Len(t, c.Resources.Limits, 2, name)
	}

	z.Spec.LiveWorker.Slots.CPU = int32p(0)
	_, err := Render(NewValues(z))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the worker needs one")
}

// The mode is off, shadow or on: anything else fails the render, and a bare
// off — which YAML reads as false — says to quote it.
func TestLiveWorkerModeIsChecked(t *testing.T) {
	for _, mode := range []interface{}{"auto", false, "ON"} {
		_, err := render(map[string]interface{}{"liveWorker": map[string]interface{}{"enabled": true, "mode": mode}}, "zaentrum", false, nil)
		require.Error(t, err, "%v", mode)
		assert.Contains(t, err.Error(), "liveWorker.mode is off, shadow or on", "%v", mode)
	}
	_, err := render(map[string]interface{}{"liveWorker": map[string]interface{}{"enabled": true, "mode": false}}, "zaentrum", false, nil)
	assert.Contains(t, err.Error(), "quote it")
}

// A plain Helm install makes the live worker's token as it makes the
// platform's other Secrets — random, kept on upgrade — and none without the
// worker or with secrets.external.
func TestLiveWorkerTokenUnderHelm(t *testing.T) {
	on := map[string]interface{}{"liveWorker": map[string]interface{}{"enabled": true}}
	a, b := secretData(t, helmRender(t, on), "zaentrum-live-worker"), secretData(t, helmRender(t, on), "zaentrum-live-worker")
	assert.Len(t, a["token"], 43)
	assert.NotEqual(t, a["token"], b["token"], "the same token for two installs")
	assert.Nil(t, find(t, helmRender(t, nil), "Secret", "zaentrum-live-worker"))
	ext := map[string]interface{}{"liveWorker": map[string]interface{}{"enabled": true}, "secrets": map[string]interface{}{"external": true}}
	assert.Nil(t, find(t, helmRender(t, ext), "Secret", "zaentrum-live-worker"))
}

// Turned off, the live worker's Deployment and Service are what the render no
// longer carries, of kinds the operator removes; chino-stream is told of it no
// more; nothing else is added or dropped.
func TestTurningTheLiveWorkerOffDropsIt(t *testing.T) {
	with, without := keys(renderCR(t, liveWorkerCR("on"))), keys(renderCR(t, func() *zaentrumv1alpha1.Zaentrum {
		z := liveWorkerCR("on")
		z.Spec.LiveWorker.Enabled = false
		return z
	}()))
	var dropped []string
	for k, o := range with {
		if _, ok := without[k]; ok {
			continue
		}
		dropped = append(dropped, k)
		removable := false
		for _, gvk := range PruneKinds {
			removable = removable || gvk.GroupKind() == o.GroupVersionKind().GroupKind()
		}
		assert.True(t, removable, "%s is not of a kind the operator removes", k)
	}
	sort.Strings(dropped)
	assert.Equal(t, []string{"Deployment/live-worker", "Service/live-worker"}, dropped)
	for k := range without {
		assert.Contains(t, with, k, "turning the live worker off adds %s", k)
	}
	assert.NotContains(t, fmt.Sprintf("%v", without["Deployment/chino-stream"].Object), "LIVE_WORKER")
}
