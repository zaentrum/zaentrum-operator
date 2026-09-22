package controller

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

const (
	selfNS      = "zaentrum-operator-system"
	selfPodName = "zaentrum-operator-controller-manager-6d9f7c4b8d-xk2mp"
	selfRS      = "zaentrum-operator-controller-manager-6d9f7c4b8d"
	selfDeploy  = "zaentrum-operator-controller-manager"
)

// selfEnv points the reconciler at a pod the way the downward API does in the
// manager Deployment.
func selfEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envPodName, selfPodName)
	t.Setenv(envPodNamespace, selfNS)
}

// managerPod is the controller's own pod, owned by its ReplicaSet the way
// Kubernetes owns it.
func managerPod(image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: selfPodName, Namespace: selfNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: replicaSetKind, Name: selfRS, UID: "rs-uid",
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: managerContainer, Image: image},
		}},
	}
}

// managerDeployment is the controller's Deployment; owners is what installed it.
func managerDeployment(owners ...metav1.OwnerReference) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: selfDeploy, Namespace: selfNS, OwnerReferences: owners,
	}}
}

func csvOwner() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: csvGroup + "/v1alpha1", Kind: csvKind,
		Name: "zaentrum-operator.v0.1.0", UID: "csv-uid",
	}
}

func selfScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(s))
	return s
}

func selfReconciler(t *testing.T, objs ...client.Object) *ZaentrumReconciler {
	t.Helper()
	s := selfScheme(t)
	return &ZaentrumReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

// ── image / version ─────────────────────────────────────────────────────────

// The version is learned from the running pod, never from a constant: the
// image is whatever the pod spec says, tag or digest.
func TestControllerReportReadsTheRunningPod(t *testing.T) {
	selfEnv(t)
	image := "ghcr.io/zaentrum/operator:sha-a3d32ba16e1c89d3c3cfcd726f85502a3da39d39"
	r := selfReconciler(t, managerPod(image), managerDeployment())

	got := r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "")

	assert.Equal(t, image, got.Image)
	assert.Equal(t, "sha-a3d32ba16e1c89d3c3cfcd726f85502a3da39d39", got.Version)
	assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest, got.Source)
	assert.False(t, got.ObservedAt.IsZero(), "a reading always carries when it was taken")
}

// A digest-pinned controller has no tag, so the short digest stands in — the
// 12-character form docker, podman and git all use.
func TestControllerReportDigestPinnedImage(t *testing.T) {
	selfEnv(t)
	image := "ghcr.io/zaentrum/operator@sha256:" +
		"1111111111222222222233333333334444444444555555555566666666667777"
	r := selfReconciler(t, managerPod(image), managerDeployment())

	got := r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "")

	assert.Equal(t, image, got.Image)
	assert.Equal(t, "111111111122", got.Version)
}

// The whole feature degrades to an honest "unknown" rather than a guess: no
// downward API (an install bundle that predates it, a local `go run`) and no
// readable pod both mean the same thing — we do not know.
func TestControllerReportPodNotReadable(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) *ZaentrumReconciler{
		"no downward API env": func(t *testing.T) *ZaentrumReconciler {
			t.Setenv(envPodName, "")
			t.Setenv(envPodNamespace, "")
			return selfReconciler(t, managerPod("ghcr.io/zaentrum/operator:latest"))
		},
		"pod gone": func(t *testing.T) *ZaentrumReconciler {
			selfEnv(t)
			return selfReconciler(t) // nothing in the cluster
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := setup(t).controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "latest")

			assert.Equal(t, "", got.Image)
			assert.Equal(t, versionUnknown, got.Version)
			assert.Equal(t, zaentrumv1alpha1.InstallSourceUnknown, got.Source)
			assert.Equal(t, "", got.AvailableUpdate,
				"nothing was read, so there is nothing to compare a channel against")
		})
	}
}

// ── source derivation ───────────────────────────────────────────────────────

func TestInstallSourceDerivation(t *testing.T) {
	image := "ghcr.io/zaentrum/operator:latest"

	t.Run("ClusterServiceVersion owns the Deployment -> olm", func(t *testing.T) {
		selfEnv(t)
		r := selfReconciler(t, managerPod(image), managerDeployment(csvOwner()))
		assert.Equal(t, zaentrumv1alpha1.InstallSourceOLM,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	t.Run("ClusterServiceVersion owns the pod -> olm", func(t *testing.T) {
		selfEnv(t)
		pod := managerPod(image)
		pod.OwnerReferences = []metav1.OwnerReference{csvOwner()}
		r := selfReconciler(t, pod)
		assert.Equal(t, zaentrumv1alpha1.InstallSourceOLM,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	t.Run("env declares the appliance -> appliance", func(t *testing.T) {
		selfEnv(t)
		t.Setenv(envInstallSource, string(zaentrumv1alpha1.InstallSourceAppliance))
		r := selfReconciler(t, managerPod(image), managerDeployment())
		assert.Equal(t, zaentrumv1alpha1.InstallSourceAppliance,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	t.Run("neither -> manifest", func(t *testing.T) {
		selfEnv(t)
		r := selfReconciler(t, managerPod(image), managerDeployment())
		assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	t.Run("no pod -> unknown", func(t *testing.T) {
		selfEnv(t)
		t.Setenv(envInstallSource, string(zaentrumv1alpha1.InstallSourceAppliance))
		r := selfReconciler(t)
		assert.Equal(t, zaentrumv1alpha1.InstallSourceUnknown,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source,
			"an env var is not evidence that anything is running")
	})

	// OLM owns the upgrade path of what it installed; an image that thinks it
	// is an appliance does not get to overrule the cluster.
	t.Run("CSV outranks the env", func(t *testing.T) {
		selfEnv(t)
		t.Setenv(envInstallSource, string(zaentrumv1alpha1.InstallSourceAppliance))
		r := selfReconciler(t, managerPod(image), managerDeployment(csvOwner()))
		assert.Equal(t, zaentrumv1alpha1.InstallSourceOLM,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	// The enum is the contract the portal and the CLI render against; a value
	// outside it is dropped, not published.
	t.Run("a value outside the enum is ignored", func(t *testing.T) {
		selfEnv(t)
		t.Setenv(envInstallSource, "helm")
		r := selfReconciler(t, managerPod(image), managerDeployment())
		assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})

	// A same-named object of some other API group is not an OLM install.
	t.Run("a non-OLM ClusterServiceVersion kind is not olm", func(t *testing.T) {
		selfEnv(t)
		owner := csvOwner()
		owner.APIVersion = "example.com/v1"
		r := selfReconciler(t, managerPod(image), managerDeployment(owner))
		assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest,
			r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "").Source)
	})
}

// ── availableUpdate ─────────────────────────────────────────────────────────

// fakeRegistry serves digest lookups for the tags it knows and 404s the rest,
// standing in for ghcr. It returns the resolver plus the host to address it by,
// so the image references below are real references to a real registry.
func fakeRegistry(t *testing.T, digests map[string]string) (*digest.Resolver, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for tag, d := range digests {
			if req.URL.Path == "/v2/zaentrum/operator/manifests/"+tag {
				w.Header().Set("Docker-Content-Digest", d)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	rv := digest.New(nil)
	rv.Scheme = "http"
	return rv, srv.Listener.Addr().String()
}

const (
	digestA = "sha256:aaaaaaaaaa22222222223333333333444444444455555555556666666666aaaa"
	digestB = "sha256:bbbbbbbbbb22222222223333333333444444444455555555556666666666bbbb"
)

func TestControllerAvailableUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("no channel target -> nothing to say", func(t *testing.T) {
		r := &ZaentrumReconciler{}
		assert.Equal(t, "", r.controllerUpdate(ctx, "ghcr.io/zaentrum/operator:v1", ""))
	})

	t.Run("already on the channel tag", func(t *testing.T) {
		r := &ZaentrumReconciler{}
		assert.Equal(t, "", r.controllerUpdate(ctx, "ghcr.io/zaentrum/operator:latest", "latest"))
	})

	// No resolver at all: a name comparison is still better than silence.
	t.Run("a different tag, no registry -> the channel tag", func(t *testing.T) {
		r := &ZaentrumReconciler{}
		assert.Equal(t, "v2", r.controllerUpdate(ctx, "ghcr.io/zaentrum/operator:v1", "v2"))
	})

	// The case a name comparison gets wrong: an operator pinned to the commit
	// tag that IS the channel's current image must not be told to update, or
	// it is told forever.
	t.Run("a different tag for the same bytes -> nothing", func(t *testing.T) {
		rv, host := fakeRegistry(t, map[string]string{"latest": digestA, "sha-abc": digestA})
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "", r.controllerUpdate(ctx, host+"/zaentrum/operator:sha-abc", "latest"))
	})

	t.Run("a different tag for different bytes -> the channel tag", func(t *testing.T) {
		rv, host := fakeRegistry(t, map[string]string{"latest": digestB, "sha-abc": digestA})
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "latest", r.controllerUpdate(ctx, host+"/zaentrum/operator:sha-abc", "latest"))
	})

	// The other case a name comparison cannot do at all: a digest-pinned
	// controller has no tag to compare, so the registry has to answer.
	t.Run("digest-pinned, channel carries newer bytes -> the channel tag", func(t *testing.T) {
		rv, host := fakeRegistry(t, map[string]string{"latest": digestB})
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "latest", r.controllerUpdate(ctx, host+"/zaentrum/operator@"+digestA, "latest"))
	})

	t.Run("digest-pinned to the channel's own bytes -> nothing", func(t *testing.T) {
		rv, host := fakeRegistry(t, map[string]string{"latest": digestA})
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "", r.controllerUpdate(ctx, host+"/zaentrum/operator@"+digestA, "latest"))
	})

	// Discovery failure is harmless by construction: a digest-pinned image
	// reports nothing rather than inventing an update out of a tag.
	t.Run("digest-pinned, registry unreachable -> nothing", func(t *testing.T) {
		rv, host := fakeRegistry(t, nil) // every lookup 404s
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "", r.controllerUpdate(ctx, host+"/zaentrum/operator@"+digestA, "latest"))
	})

	t.Run("tagged, registry unreachable -> falls back to the tag comparison", func(t *testing.T) {
		rv, host := fakeRegistry(t, nil)
		r := &ZaentrumReconciler{Digest: rv}
		assert.Equal(t, "latest", r.controllerUpdate(ctx, host+"/zaentrum/operator:v1", "latest"))
	})

	t.Run("an unparseable image says nothing", func(t *testing.T) {
		r := &ZaentrumReconciler{}
		assert.Equal(t, "", r.controllerUpdate(ctx, "", "latest"))
	})
}

// Discovery failure must cost the reading, never the reconcile.
func TestControllerReportSurvivesDiscoveryFailure(t *testing.T) {
	selfEnv(t)
	rv, host := fakeRegistry(t, nil) // the registry 404s everything
	r := selfReconciler(t, managerPod(host+"/zaentrum/operator@"+digestA), managerDeployment())
	r.Digest = rv

	got := r.controllerReport(context.Background(), &zaentrumv1alpha1.Zaentrum{}, "latest")

	assert.Equal(t, "", got.AvailableUpdate)
	assert.Equal(t, "aaaaaaaaaa22", got.Version, "the reading itself survives")
	assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest, got.Source)
}

// ── observedAt ──────────────────────────────────────────────────────────────

// The timestamp marks a change, not a poll. Moving it every pass would make
// every status write a real change, and the CR watch would turn each one back
// into the reconcile that wrote it.
func TestObservedAtHoldsWhileTheReadingIsUnchanged(t *testing.T) {
	selfEnv(t)
	r := selfReconciler(t, managerPod("ghcr.io/zaentrum/operator:latest"), managerDeployment())

	z := &zaentrumv1alpha1.Zaentrum{}
	first := r.controllerReport(context.Background(), z, "")
	z.Status.Controller = first

	time.Sleep(2 * time.Millisecond)
	second := r.controllerReport(context.Background(), z, "")

	assert.Equal(t, first.ObservedAt, second.ObservedAt)
}

func TestObservedAtMovesWhenTheReadingChanges(t *testing.T) {
	selfEnv(t)
	r := selfReconciler(t, managerPod("ghcr.io/zaentrum/operator:v2"), managerDeployment())

	z := &zaentrumv1alpha1.Zaentrum{Status: zaentrumv1alpha1.ZaentrumStatus{
		Controller: &zaentrumv1alpha1.ControllerStatus{
			Image:      "ghcr.io/zaentrum/operator:v1",
			Version:    "v1",
			Source:     zaentrumv1alpha1.InstallSourceManifest,
			ObservedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
	}}

	got := r.controllerReport(context.Background(), z, "")

	assert.Equal(t, "v2", got.Version)
	assert.True(t, got.ObservedAt.After(z.Status.Controller.ObservedAt.Time))
}

// ── the reconcile actually writes it ────────────────────────────────────────

// The wiring is the part that can silently do nothing, so reconcile the CR for
// real: render, apply, refresh, and check the operator published itself beside
// the platform it just rolled out.
func TestReconcileWritesTheControllerReport(t *testing.T) {
	selfEnv(t)

	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"channels":{"stable":"v9"}}`))
	}))
	t.Cleanup(releases.Close)

	z := &zaentrumv1alpha1.Zaentrum{
		ObjectMeta: metav1.ObjectMeta{Name: "zaentrum", Namespace: "zaentrum", UID: "z-uid", Generation: 3},
	}
	z.Spec.Features.Kafka = true

	s := selfScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(z, managerPod("ghcr.io/zaentrum/operator:v1"), managerDeployment()).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyAsCreateOrUpdate}).
		Build()
	r := &ZaentrumReconciler{
		Client:      c,
		Scheme:      s,
		ReleasesURL: releases.URL,
		Updates:     updates.Client{HTTP: releases.Client()},
	}

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "zaentrum", Name: "zaentrum"}})
	require.NoError(t, err)

	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "zaentrum", Name: "zaentrum"}, &got))

	require.NotNil(t, got.Status.Controller, "the operator says nothing about itself")
	assert.Equal(t, "ghcr.io/zaentrum/operator:v1", got.Status.Controller.Image)
	assert.Equal(t, "v1", got.Status.Controller.Version)
	assert.Equal(t, zaentrumv1alpha1.InstallSourceManifest, got.Status.Controller.Source)
	assert.Equal(t, "v9", got.Status.Controller.AvailableUpdate,
		"the channel the platform reads is the channel the controller is measured against")

	// The platform's own status is what it always was: the report sits beside
	// it, not on top of it. Manual mode still renders "latest" and surfaces the
	// channel target as the PLATFORM's available update.
	assert.Equal(t, "latest", got.Status.CurrentVersion)
	assert.Equal(t, "v9", got.Status.AvailableUpdate)
	assert.Equal(t, int64(3), got.Status.ObservedGeneration)
	assert.NotEmpty(t, got.Status.Phase)
	assert.NotEmpty(t, got.Status.Components)
	require.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, condTypeApplied))
	assert.Equal(t, metav1.ConditionTrue,
		meta.FindStatusCondition(got.Status.Conditions, condTypeApplied).Status)
}

// A pinned spec.version opts the CR out of channel tracking — for the platform
// and, inseparably, for the controller: an air-gapped pinned install makes no
// outbound call, so there is nothing it could claim to know about updates.
func TestReconcilePinnedVersionConsultsNoChannel(t *testing.T) {
	selfEnv(t)

	consulted := false
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		consulted = true
		_, _ = w.Write([]byte(`{"channels":{"stable":"v9"}}`))
	}))
	t.Cleanup(releases.Close)

	z := &zaentrumv1alpha1.Zaentrum{
		ObjectMeta: metav1.ObjectMeta{Name: "zaentrum", Namespace: "zaentrum", UID: "z-uid"},
	}
	z.Spec.Features.Kafka = true
	z.Spec.Version = "v1"

	s := selfScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(z, managerPod("ghcr.io/zaentrum/operator:v1"), managerDeployment()).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyAsCreateOrUpdate}).
		Build()
	r := &ZaentrumReconciler{
		Client:      c,
		Scheme:      s,
		ReleasesURL: releases.URL,
		Updates:     updates.Client{HTTP: releases.Client()},
	}

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "zaentrum", Name: "zaentrum"}})
	require.NoError(t, err)

	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "zaentrum", Name: "zaentrum"}, &got))

	assert.False(t, consulted, "a pinned CR must not reach for the channel document")
	require.NotNil(t, got.Status.Controller)
	assert.Equal(t, "v1", got.Status.Controller.Version)
	assert.Equal(t, "", got.Status.Controller.AvailableUpdate)
}

// ── the install bundles have to inject the downward API ─────────────────────

// managerEnv returns the manager container's env in one install bundle, as
// name -> "value" or "fieldRef:<path>". podSpec picks the pod spec out of a
// document, because the OLM bundle carries the Deployment inside the CSV.
func managerEnv(t *testing.T, file string, podSpec func(doc map[string]any) map[string]any) map[string]string {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)

	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		doc := map[string]any{}
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		spec := podSpec(doc)
		if spec == nil {
			continue
		}
		containers, _ := spec["containers"].([]any)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			if cm["name"] != managerContainer {
				continue
			}
			out := map[string]string{}
			env, _ := cm["env"].([]any)
			for _, e := range env {
				em, _ := e.(map[string]any)
				name, _ := em["name"].(string)
				if v, ok := em["value"].(string); ok {
					out[name] = v
					continue
				}
				from, _ := em["valueFrom"].(map[string]any)
				ref, _ := from["fieldRef"].(map[string]any)
				if p, ok := ref["fieldPath"].(string); ok {
					out[name] = "fieldRef:" + p
				}
			}
			return out
		}
	}
	t.Fatalf("%s: no %q container in any manager Deployment", file, managerContainer)
	return nil
}

// deploymentPodSpec picks the controller-manager Deployment's pod spec.
func deploymentPodSpec(doc map[string]any) map[string]any {
	if doc["kind"] != "Deployment" {
		return nil
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta["name"] != selfDeploy {
		return nil
	}
	spec, _ := doc["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	return pod
}

// csvPodSpec digs the same Deployment out of the OLM install strategy.
func csvPodSpec(doc map[string]any) map[string]any {
	if doc["kind"] != "ClusterServiceVersion" {
		return nil
	}
	spec, _ := doc["spec"].(map[string]any)
	install, _ := spec["install"].(map[string]any)
	istrat, _ := install["spec"].(map[string]any)
	deps, _ := istrat["deployments"].([]any)
	for _, d := range deps {
		dm, _ := d.(map[string]any)
		if dm["name"] != selfDeploy {
			continue
		}
		dspec, _ := dm["spec"].(map[string]any)
		tmpl, _ := dspec["template"].(map[string]any)
		pod, _ := tmpl["spec"].(map[string]any)
		return pod
	}
	return nil
}

// Without the downward API the operator cannot read its own pod, and the whole
// report silently degrades to "unknown" — in whichever bundle forgot it. Every
// copy of the manager Deployment is checked, because they are maintained
// separately and only one of them is the one a given cluster installed from.
func TestInstallBundlesInjectTheDownwardAPI(t *testing.T) {
	bundles := map[string]struct {
		file    string
		podSpec func(map[string]any) map[string]any
		source  string // the install source this bundle declares, "" for none
	}{
		"config/manager": {
			file: "../../config/manager/manager.yaml", podSpec: deploymentPodSpec,
		},
		"deploy/operator-install.yaml": {
			file: "../../../deploy/operator-install.yaml", podSpec: deploymentPodSpec,
		},
		"OLM bundle CSV": {
			file: "../../bundle/manifests/zaentrum-operator.clusterserviceversion.yaml", podSpec: csvPodSpec,
		},
		// The appliance is the one install nothing in the cluster distinguishes
		// from a manifest apply, so its render says so itself.
		"all-in-one render": {
			file: "../../../deploy/allinone/manifests/10-operator.yaml", podSpec: deploymentPodSpec,
			source: string(zaentrumv1alpha1.InstallSourceAppliance),
		},
	}

	for name, b := range bundles {
		t.Run(name, func(t *testing.T) {
			env := managerEnv(t, b.file, b.podSpec)

			assert.Equal(t, "fieldRef:metadata.name", env[envPodName])
			assert.Equal(t, "fieldRef:metadata.namespace", env[envPodNamespace])

			// A bundle that is not the appliance must not claim to be one —
			// the source is derived precisely so an installer cannot mislabel
			// itself by accident.
			assert.Equal(t, b.source, env[envInstallSource])
		})
	}
}

// ── the rest of the status is untouched ─────────────────────────────────────

// status.controller is a reading placed beside the platform's status; it must
// not move the phase, the conditions, currentVersion or a component.
func TestControllerReportDisturbsNothingElse(t *testing.T) {
	selfEnv(t)
	r := selfReconciler(t, managerPod("ghcr.io/zaentrum/operator:latest"), managerDeployment())

	z := &zaentrumv1alpha1.Zaentrum{Status: zaentrumv1alpha1.ZaentrumStatus{
		Phase:              "Ready",
		CurrentVersion:     "latest",
		AvailableUpdate:    "v2",
		ObservedGeneration: 7,
		Components:         []zaentrumv1alpha1.ComponentStatus{{Name: "chino-api", Ready: true, Image: "x"}},
		Conditions: []metav1.Condition{{
			Type: condTypeReady, Status: metav1.ConditionTrue, Reason: "AllComponentsReady",
			Message: "all components are ready", LastTransitionTime: metav1.Now(),
		}},
	}}
	before := *z.Status.DeepCopy()

	z.Status.Controller = r.controllerReport(context.Background(), z, "")

	require.NotNil(t, z.Status.Controller)
	z.Status.Controller = nil
	assert.Equal(t, before, z.Status, "only status.controller changed")
}
