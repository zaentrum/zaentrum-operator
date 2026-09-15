package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/addon"
)

const addonNS = "zaentrum-beta"

// staticCharts serves one archive (or error) for every chart ref.
type staticCharts struct {
	archive *addon.Archive
	err     error
}

func (s *staticCharts) Fetch(context.Context, zaentrumv1alpha1.AddonChart, bool) (*addon.Archive, error) {
	return s.archive, s.err
}

// exampleCharts packages the neutral example chart from the addon testdata.
func exampleCharts(t *testing.T) *staticCharts {
	t.Helper()
	chrt, err := loader.LoadDir("../addon/testdata/example")
	require.NoError(t, err)
	path, err := chartutil.Save(chrt, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return &staticCharts{archive: &addon.Archive{Data: data, Digest: addon.Digest(data)}}
}

// applyAsCreateOrUpdate stands in for server-side apply, which the fake
// client does not implement: create the object, or replace it.
func applyAsCreateOrUpdate(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return c.Patch(ctx, obj, patch, opts...)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("apply expects an unstructured object, got %T", obj)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(u.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(u), live)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, u)
	}
	if err != nil {
		return err
	}
	u.SetResourceVersion(live.GetResourceVersion())
	return c.Update(ctx, u)
}

func testPlatform() *zaentrumv1alpha1.Zaentrum {
	z := &zaentrumv1alpha1.Zaentrum{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum", Namespace: addonNS, UID: "platform-uid"}}
	z.Spec.Hostname = "zaentrum.beta.example.org"
	z.Spec.Features.Kafka = true
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	z.Spec.ImagePullSecrets = []string{"registry-pull"}
	return z
}

func testAddon(suspend bool) *zaentrumv1alpha1.ZaentrumAddon {
	a := &zaentrumv1alpha1.ZaentrumAddon{ObjectMeta: metav1.ObjectMeta{
		Name: "example", Namespace: addonNS, UID: "addon-uid", Generation: 1,
	}}
	a.Spec.Chart = zaentrumv1alpha1.AddonChart{Ref: "oci://registry.example.org/charts/example", Version: "0.1.0"}
	a.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"greeting":"hi"}`)}
	a.Spec.ValuesFrom = []zaentrumv1alpha1.AddonValuesReference{{
		Kind: "Secret", Name: "zaentrum-addon-example-values", ValuesKey: "auth.token", TargetPath: "auth.token",
	}}
	a.Spec.Suspend = suspend
	return a
}

func testValuesSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "zaentrum-addon-example-values", Namespace: addonNS,
			Labels: map[string]string{addon.LabelAddon: "example"},
		},
		Data: map[string][]byte{"auth.token": []byte("t0ken")},
	}
}

// ownedBy returns a controller owner reference to the object.
func ownedBy(kind, name string, uid types.UID) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{
		APIVersion: zaentrumv1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: &yes,
	}}
}

func newAddonReconciler(t *testing.T, charts ChartSource, objs ...client.Object) (*ZaentrumAddonReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&zaentrumv1alpha1.ZaentrumAddon{}, &zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyAsCreateOrUpdate}).
		Build()
	return &ZaentrumAddonReconciler{Client: c, Scheme: scheme, APIReader: c, Charts: charts}, c
}

func reconcileExample(t *testing.T, r *ZaentrumAddonReconciler) (ctrl.Result, *zaentrumv1alpha1.ZaentrumAddon) {
	t.Helper()
	key := types.NamespacedName{Namespace: addonNS, Name: "example"}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	var a zaentrumv1alpha1.ZaentrumAddon
	require.NoError(t, r.Get(context.Background(), key, &a))
	return res, &a
}

func objectKeys(objs []zaentrumv1alpha1.AddonObject) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.Kind+"/"+o.Name)
	}
	return out
}

// suspend: plan only. The plan is reported, generated values are stored and
// the values Secret adopted — but not a single chart object is applied.
func TestAddonPlanOnly(t *testing.T) {
	charts := exampleCharts(t)
	r, c := newAddonReconciler(t, charts, testPlatform(), testAddon(true), testValuesSecret())
	ctx := context.Background()

	res, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonPlanned, a.Status.Phase, a.Status.Message)
	assert.Equal(t, addonRequeueSettled, res.RequeueAfter)
	assert.EqualValues(t, 1, a.Status.ObservedGeneration)
	require.NotNil(t, a.Status.Plan)
	plan := a.Status.Plan
	assert.Empty(t, plan.Violations)
	assert.Empty(t, plan.ValuesErrors)
	assert.Equal(t, "example", plan.Chart.Name)
	assert.Equal(t, charts.archive.Digest, plan.Chart.Digest)
	assert.Equal(t, "worker", plan.Chart.Annotations["zaentrum.io/primary"])
	assert.Contains(t, plan.ValuesSchema, `"writeOnly": true`)
	assert.Equal(t, []string{"Deployment/worker", "Secret/worker", "Service/worker"}, objectKeys(plan.Objects))
	assert.Equal(t, []zaentrumv1alpha1.AddonWorkload{{
		Kind: "Deployment", Name: "worker", Images: []string{"registry.example.org/example/worker:1.0.0"}, Ports: []int32{8080},
	}}, plan.Workloads)
	assert.Equal(t, &zaentrumv1alpha1.AddonPlanChanges{Added: []string{"Deployment/worker", "Secret/worker", "Service/worker"}}, plan.Changes)
	assert.True(t, meta.IsStatusConditionTrue(a.Status.Conditions, condTypePlanned))
	assert.Equal(t, "Suspended", meta.FindStatusCondition(a.Status.Conditions, condTypeReady).Reason)
	assert.Nil(t, a.Status.LastAppliedChart)

	var generated corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "zaentrum-addon-example-generated"}, &generated))
	key, err := base64.StdEncoding.DecodeString(string(generated.Data["auth.signingKey"]))
	require.NoError(t, err)
	assert.Len(t, key, 32)
	assert.Equal(t, "example", generated.Labels[addon.LabelAddon])
	require.NotNil(t, metav1.GetControllerOf(&generated))
	assert.EqualValues(t, "addon-uid", metav1.GetControllerOf(&generated).UID)

	var values corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "zaentrum-addon-example-values"}, &values))
	require.Len(t, values.OwnerReferences, 1, "the values Secret is adopted")
	assert.EqualValues(t, "addon-uid", values.OwnerReferences[0].UID)

	var dep appsv1.Deployment
	err = c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &dep)
	assert.True(t, apierrors.IsNotFound(err), "nothing is applied while suspended")

	// A second plan never generates the stored value again.
	_, _ = reconcileExample(t, r)
	var again corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "zaentrum-addon-example-generated"}, &again))
	assert.Equal(t, generated.Data["auth.signingKey"], again.Data["auth.signingKey"])
}

// Install: server-side apply with owner references, labels, the values
// checksum and pod security defaults; prune what the chart no longer renders;
// Installing until the worker is ready, then Ready.
func TestAddonInstall(t *testing.T) {
	charts := exampleCharts(t)
	legacy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-legacy", Namespace: addonNS,
		Labels:          map[string]string{addon.LabelAddon: "example", addon.LabelManagedBy: addon.ManagedBy},
		OwnerReferences: ownedBy("ZaentrumAddon", "example", "addon-uid"),
	}}
	// Labelled for the addon by someone else: never pruned.
	handmade := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-notes", Namespace: addonNS, Labels: map[string]string{addon.LabelAddon: "example"},
	}}
	r, c := newAddonReconciler(t, charts, testPlatform(), testAddon(false), testValuesSecret(), legacy, handmade)
	ctx := context.Background()

	res, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonInstalling, a.Status.Phase, a.Status.Message)
	assert.Equal(t, addonRequeueRollout, res.RequeueAfter)
	assert.Equal(t, &zaentrumv1alpha1.AddonChart{
		Ref: "oci://registry.example.org/charts/example", Version: "0.1.0", Digest: charts.archive.Digest,
	}, a.Status.LastAppliedChart)
	assert.Equal(t, []string{"ConfigMap/worker-legacy"}, a.Status.Plan.Changes.Removed)
	require.Len(t, a.Status.Components, 1)
	assert.Equal(t, "worker", a.Status.Components[0].Name)
	assert.EqualValues(t, 0, a.Status.Components[0].Ready)
	assert.EqualValues(t, 1, a.Status.Components[0].Desired)

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &dep))
	assert.Equal(t, map[string]string{
		"app.kubernetes.io/name":       "worker",
		"zaentrum.io/addon":            "example",
		"zaentrum.io/component":        "worker",
		"app.kubernetes.io/managed-by": "zaentrum-operator",
		"app.kubernetes.io/instance":   "example",
		"app.kubernetes.io/part-of":    "zaentrum-beta-addons",
	}, dep.Labels)
	owner := metav1.GetControllerOf(&dep)
	require.NotNil(t, owner)
	assert.Equal(t, "ZaentrumAddon", owner.Kind)
	assert.EqualValues(t, "addon-uid", owner.UID)
	pod := dep.Spec.Template
	assert.Equal(t, "worker", pod.Labels["zaentrum.io/component"])
	assert.Len(t, pod.Annotations["zaentrum.io/values-checksum"], 64)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)
	container := pod.Spec.Containers[0]
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry-pull"}}, pod.Spec.ImagePullSecrets)
	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "https://sso.example.org/realms/x", env["OIDC_ISSUER"])
	assert.Equal(t, "kafka:9092", env["EVENT_BROKERS"])
	assert.Equal(t, "hi", env["GREETING"])

	var credentials corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &credentials))
	assert.Equal(t, "t0ken", string(credentials.Data["token"]), "the secret input reached the chart")
	var svc corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &svc))

	err := c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker-legacy"}, &corev1.ConfigMap{})
	assert.True(t, apierrors.IsNotFound(err), "an applied object the chart dropped is pruned")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker-notes"}, &corev1.ConfigMap{}),
		"only objects the operator applied are pruned")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "zaentrum-addon-example-values"}, &corev1.Secret{}),
		"values objects are never pruned")

	// The worker becomes available.
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	require.NoError(t, c.Status().Update(ctx, &dep))

	res, a = reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonReady, a.Status.Phase, a.Status.Message)
	assert.Equal(t, addonRequeueSettled, res.RequeueAfter)
	assert.Equal(t, []zaentrumv1alpha1.AddonComponentStatus{{Name: "worker", Kind: "Deployment", Ready: 1, Desired: 1}}, a.Status.Components)
	assert.True(t, meta.IsStatusConditionTrue(a.Status.Conditions, condTypeReady))
	assert.Empty(t, a.Status.Plan.Changes.Added, "the plan now matches what is applied")
}

// A rollout that stopped making progress after the addon was installed is
// Degraded, with the pod's reason on the component.
func TestAddonDegraded(t *testing.T) {
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(false), testValuesSecret())
	ctx := context.Background()
	_, _ = reconcileExample(t, r)

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &dep))
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1}
	require.NoError(t, c.Status().Update(ctx, &dep))
	require.NoError(t, c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-abc", Namespace: addonNS, Labels: map[string]string{"app.kubernetes.io/name": "worker"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "registry.example.org/example/worker:1.0.0"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "worker",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: "container has runAsNonRoot and image will run as root"}},
		}}},
	}))

	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonDegraded, a.Status.Phase, a.Status.Message)
	require.Len(t, a.Status.Components, 1)
	assert.Equal(t, "CreateContainerConfigError: container has runAsNonRoot and image will run as root", a.Status.Components[0].Reason)
	assert.Contains(t, a.Status.Message, "worker 0/1")
}

func TestAddonNoPlatform(t *testing.T) {
	r, _ := newAddonReconciler(t, exampleCharts(t), testAddon(true), testValuesSecret())
	res, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	assert.Equal(t, "no platform in this namespace", a.Status.Message)
	assert.Nil(t, a.Status.Plan)
	assert.Equal(t, addonRequeueRetry, res.RequeueAfter)
}

// A name taken by an object the addon does not control is a violation, and
// nothing at all is applied — never forced over the platform.
func TestAddonRefusesCollision(t *testing.T) {
	platformSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "worker", Namespace: addonNS, Labels: map[string]string{"app": "platform"},
		OwnerReferences: ownedBy("Zaentrum", "zaentrum", "platform-uid"),
	}}
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(false), testValuesSecret(), platformSvc)
	ctx := context.Background()

	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	assert.Equal(t, []string{"Service/worker: already exists and is not owned by this addon"}, a.Status.Plan.Violations)
	assert.Equal(t, "Service/worker: already exists and is not owned by this addon", a.Status.Message)
	assert.False(t, meta.IsStatusConditionTrue(a.Status.Conditions, condTypePlanned))

	err := c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err), "nothing is applied")
	var svc corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: addonNS, Name: "worker"}, &svc))
	assert.Equal(t, map[string]string{"app": "platform"}, svc.Labels, "the platform's Service is untouched")
}

// A missing secret input is a values error: PlanFailed while suspended.
func TestAddonValuesErrors(t *testing.T) {
	r, _ := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true))
	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonPlanFailed, a.Status.Phase)
	require.NotNil(t, a.Status.Plan)
	assert.Contains(t, a.Status.Plan.ValuesErrors, "valuesFrom Secret/zaentrum-addon-example-values: not found")
	assert.Contains(t, a.Status.Plan.ValuesErrors, "auth: token is required")
	assert.Empty(t, a.Status.Plan.Objects, "nothing rendered")
	assert.EqualValues(t, 1, a.Status.ObservedGeneration)
}

func TestAddonFetchFailure(t *testing.T) {
	r, _ := newAddonReconciler(t, &staticCharts{err: errors.New("pull chart: unavailable")}, testPlatform(), testAddon(false))
	res, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonFailed, a.Status.Phase)
	assert.Equal(t, "pull chart: unavailable", a.Status.Message)
	assert.Nil(t, a.Status.Plan)
	assert.Equal(t, addonRequeueRetry, res.RequeueAfter)
	assert.Equal(t, "FetchFailed", meta.FindStatusCondition(a.Status.Conditions, condTypePlanned).Reason)
}

// zaentrum.io/keep=true releases a values Secret: removing the addon then
// leaves it behind.
func TestAddonKeepReleasesValues(t *testing.T) {
	values := testValuesSecret()
	values.Labels[addon.LabelKeep] = "true"
	values.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: zaentrumv1alpha1.GroupVersion.String(), Kind: "ZaentrumAddon", Name: "example", UID: "addon-uid",
	}}
	r, c := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true), values)
	_, _ = reconcileExample(t, r)

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: values.Name}, &got))
	assert.Empty(t, got.OwnerReferences)
}

func TestAddonWatchMapping(t *testing.T) {
	other := testAddon(true)
	other.Name, other.UID = "other", "other-uid"
	other.Spec.ValuesFrom = nil
	r, _ := newAddonReconciler(t, exampleCharts(t), testPlatform(), testAddon(true), other)
	ctx := context.Background()

	secret := func(name string) client.Object {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS}}
	}
	names := func(reqs []ctrl.Request) []string {
		var out []string
		for _, req := range reqs {
			out = append(out, req.Name)
		}
		return out
	}
	assert.Equal(t, []string{"example"}, names(r.addonsReading("Secret")(ctx, secret("zaentrum-addon-example-values"))))
	assert.Equal(t, []string{"other"}, names(r.addonsReading("Secret")(ctx, secret("zaentrum-addon-other-generated"))))
	assert.Empty(t, r.addonsReading("ConfigMap")(ctx, secret("zaentrum-addon-example-values")), "kinds must match")
	assert.ElementsMatch(t, []string{"example", "other"}, names(r.addonsInNamespace(ctx, testPlatform())))
}

// A Job's pod template is immutable: a Job the chart changed is replaced, so
// an upgrade or a values change runs it again instead of failing the apply.
func TestAddonReplacesChangedJob(t *testing.T) {
	chrt, err := loader.LoadDir("../addon/testdata/example")
	require.NoError(t, err)
	chrt.Templates = append(chrt.Templates, &chart.File{Name: "templates/setup-job.yaml", Data: []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: setup
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: setup
          image: {{ .Values.image }}
`)})
	path, err := chartutil.Save(chrt, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	charts := &staticCharts{archive: &addon.Archive{Data: data, Digest: addon.Digest(data)}}

	previous := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "setup", Namespace: addonNS,
		Labels:          map[string]string{addon.LabelAddon: "example", addon.LabelManagedBy: addon.ManagedBy},
		OwnerReferences: ownedBy("ZaentrumAddon", "example", "addon-uid"),
	}}
	var rejected, deleted bool
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, zaentrumv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testPlatform(), testAddon(false), testValuesSecret(), previous).
		WithStatusSubresource(&zaentrumv1alpha1.ZaentrumAddon{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() == types.ApplyPatchType && obj.GetName() == "setup" && !deleted {
					rejected = true
					return apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "Job"}, "setup", field.ErrorList{
						field.Invalid(field.NewPath("spec", "template"), "…", "field is immutable"),
					})
				}
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "setup" {
					deleted = true
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &ZaentrumAddonReconciler{Client: c, Scheme: scheme, APIReader: c, Charts: charts}

	_, a := reconcileExample(t, r)
	assert.Equal(t, zaentrumv1alpha1.AddonInstalling, a.Status.Phase, a.Status.Message)
	assert.True(t, rejected)
	assert.True(t, deleted, "the old Job is deleted")
	var job batchv1.Job
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: addonNS, Name: "setup"}, &job))
	assert.Equal(t, "registry.example.org/example/worker:1.0.0", job.Spec.Template.Spec.Containers[0].Image, "and applied again")
	assert.Len(t, job.Spec.Template.Annotations["zaentrum.io/values-checksum"], 64)
}
