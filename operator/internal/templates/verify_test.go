package templates

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// renderSplit renders z and splits the test hooks off, as the reconciler does.
func renderSplit(t *testing.T, z *zaentrumv1alpha1.Zaentrum) (platform, tests []*unstructured.Unstructured) {
	t.Helper()
	objs, err := Render(NewValues(z))
	require.NoError(t, err)
	return SplitTestHooks(objs)
}

// verifyJob renders z and returns its verification Job, typed.
func verifyJob(t *testing.T, z *zaentrumv1alpha1.Zaentrum) *batchv1.Job {
	t.Helper()
	_, tests := renderSplit(t, z)
	u := VerifyJob(tests)
	require.NotNil(t, u, "the render has no verification Job")
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &job))
	return &job
}

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

// secretRef asserts env var name is read from secret/key, optionally.
func secretRef(t *testing.T, env map[string]corev1.EnvVar, name, secret, key string) {
	t.Helper()
	e, ok := env[name]
	require.True(t, ok, "env %s missing", name)
	assert.Empty(t, e.Value, "%s must come from a Secret, never a literal", name)
	require.NotNil(t, e.ValueFrom, name)
	require.NotNil(t, e.ValueFrom.SecretKeyRef, name)
	assert.Equal(t, secret, e.ValueFrom.SecretKeyRef.Name, name)
	assert.Equal(t, key, e.ValueFrom.SecretKeyRef.Key, name)
	require.NotNil(t, e.ValueFrom.SecretKeyRef.Optional, name)
	assert.True(t, *e.ValueFrom.SecretKeyRef.Optional,
		"%s: an absent Secret must not wedge the pod in CreateContainerConfigError until the deadline", name)
}

// The chart's checks are a test hook; the operator must never apply them with
// the platform, and nothing else may be lost on the way.
func TestSplitTestHooksTakesOnlyTheHooks(t *testing.T) {
	all, err := Render(NewValues(base("zaentrum")))
	require.NoError(t, err)
	platform, tests := SplitTestHooks(all)

	assert.Len(t, platform, len(all)-len(tests), "every object lands on exactly one side")
	require.NotNil(t, VerifyJob(tests), "the verification Job is a test hook")
	for _, o := range platform {
		assert.False(t, IsTestHook(o), "%s/%s is a test hook among the applied objects", o.GetKind(), o.GetName())
		assert.False(t, o.GetName() == VerifyJobName && (o.GetKind() == "Job" || o.GetKind() == "Secret"),
			"%s/%s belongs to the checks, not the platform", o.GetKind(), o.GetName())
	}
	for _, o := range tests {
		assert.True(t, IsTestHook(o))
	}
	assert.NotNil(t, find(t, platform, "Deployment", "keycloak"), "the platform itself is untouched")
	assert.Equal(t, count(all, "Deployment"), count(platform, "Deployment"))
}

func TestIsTestHook(t *testing.T) {
	for hook, want := range map[string]bool{
		"test":                      true,
		"test-success":              true, // Helm 2's spelling, still honoured by Helm
		"pre-install,test":          true,
		" test ":                    true,
		"pre-install":               false,
		"post-upgrade,post-install": false,
		"":                          false,
	} {
		o := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if hook != "" {
			o.SetAnnotations(map[string]string{"helm.sh/hook": hook})
		}
		assert.Equal(t, want, IsTestHook(o), "helm.sh/hook=%q", hook)
	}
}

// The Job a run starts from: restricted-SCC friendly, one attempt, a deadline,
// the issuer's split-horizon entries, the zae doctor command line of the
// contract, credentials from Secret zaentrum-verify only.
func TestVerifyJobRendersTheCheck(t *testing.T) {
	z := demoCR("zaentrum-demo")
	job := verifyJob(t, z)
	spec := job.Spec.Template.Spec

	assert.Equal(t, "test", job.Annotations["helm.sh/hook"], "plain `helm test` runs it")
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(600), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished, "the backstop for when nobody deletes the Job")
	assert.Equal(t, corev1.RestartPolicyNever, spec.RestartPolicy)
	require.NotNil(t, spec.AutomountServiceAccountToken)
	assert.False(t, *spec.AutomountServiceAccountToken, "the checks never talk to the Kubernetes API")

	// The same hostAliases the issuer-validating services get.
	platform, _ := renderSplit(t, z)
	api := find(t, platform, "Deployment", "chino-api")
	require.NotNil(t, api)
	var validator appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(api.Object, &validator))
	require.NotEmpty(t, validator.Spec.Template.Spec.HostAliases)
	assert.Equal(t, validator.Spec.Template.Spec.HostAliases, spec.HostAliases)
	require.Len(t, spec.HostAliases, 1)
	assert.Equal(t, z.Spec.Network.IssuerHostAliasIP, spec.HostAliases[0].IP)
	assert.Equal(t, []string{z.Spec.Hostname}, spec.HostAliases[0].Hostnames)
	assert.Equal(t, validator.Spec.Template.Spec.ImagePullSecrets, spec.ImagePullSecrets)

	require.NotNil(t, spec.SecurityContext)
	require.NotNil(t, spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, spec.SecurityContext.SeccompProfile.Type)
	assert.Nil(t, spec.SecurityContext.RunAsUser, "a fixed UID is refused by the restricted SCC")

	all := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	require.Len(t, all, 2, "the account init container and the check")
	for _, c := range all {
		sc := c.SecurityContext
		require.NotNil(t, sc, c.Name)
		require.NotNil(t, sc.AllowPrivilegeEscalation, c.Name)
		assert.False(t, *sc.AllowPrivilegeEscalation, c.Name)
		require.NotNil(t, sc.Capabilities, c.Name)
		assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, c.Name)
		require.NotNil(t, sc.ReadOnlyRootFilesystem, c.Name)
		assert.True(t, *sc.ReadOnlyRootFilesystem, c.Name)
		assert.Nil(t, sc.Privileged, c.Name)
		assert.Nil(t, sc.RunAsUser, c.Name)
		assert.Equal(t, corev1.TerminationMessageReadFile, c.TerminationMessagePolicy, c.Name)
		assert.False(t, c.Resources.Limits.Memory().IsZero(), "%s has a memory limit", c.Name)
		assert.False(t, c.Resources.Limits.Cpu().IsZero(), "%s has a CPU limit", c.Name)
		assert.False(t, c.Resources.Requests.Memory().IsZero(), "%s has a memory request", c.Name)
	}

	doctor := spec.Containers[0]
	assert.Equal(t, VerifyCheckContainer, doctor.Name)
	assert.Equal(t, "ghcr.io/zaentrum/zae:latest", doctor.Image, "one of our images, on the platform's tag")
	assert.Empty(t, doctor.Command, "the image's entrypoint is zae")
	assert.Equal(t, []string{"doctor", "--url", "https://" + z.Spec.Hostname, "--sign-in",
		"--report", "/dev/termination-log"}, doctor.Args)
	env := envByName(doctor)
	assert.Len(t, env, 2)
	secretRef(t, env, "ZAE_DOCTOR_USER", VerifySecretName, "username")
	secretRef(t, env, "ZAE_DOCTOR_PASSWORD", VerifySecretName, "password")
}

// With bundled identity an init container prepares the account, from the very
// Keycloak image the platform runs, with the script the chart ships.
func TestVerifyJobPreparesTheBundledAccount(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Keycloak.Image = "registry.example.org/identity/keycloak:26.0.7-custom"
	job := verifyJob(t, z)
	spec := job.Spec.Template.Spec
	require.Len(t, spec.InitContainers, 1)
	account := spec.InitContainers[0]
	assert.Equal(t, VerifyAccountContainer, account.Name)

	platform, _ := renderSplit(t, z)
	kc := find(t, platform, "Deployment", "keycloak")
	require.NotNil(t, kc)
	kcImage, _, _ := unstructured.NestedSlice(kc.Object, "spec", "template", "spec", "containers")
	assert.Equal(t, kcImage[0].(map[string]interface{})["image"], account.Image,
		"the same image the platform's Keycloak runs")
	assert.Equal(t, z.Spec.Keycloak.Image, account.Image)

	script, err := os.ReadFile("../../platform/chart/files/verify-account.sh")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/bash", "-c"}, account.Command)
	require.Len(t, account.Args, 1)
	assert.Equal(t, string(script), account.Args[0], "the script runs exactly as shipped")

	env := envByName(account)
	assert.Equal(t, "http://keycloak:80/auth", env["KC_SERVER"].Value, "the in-cluster admin API")
	secretRef(t, env, "KC_ADMIN_USER", "zaentrum-keycloak-admin", "username")
	secretRef(t, env, "KC_CLI_PASSWORD", "zaentrum-keycloak-admin", "password")
	secretRef(t, env, "VERIFY_USERNAME", VerifySecretName, "username")
	secretRef(t, env, "VERIFY_PASSWORD", VerifySecretName, "password")
	assert.NotEmpty(t, env["VERIFY_EMAIL"].Value)

	require.Len(t, account.VolumeMounts, 1, "kcadm keeps its session in a scratch dir")
	assert.Equal(t, "/tmp", account.VolumeMounts[0].MountPath)
	require.Len(t, spec.Volumes, 1)
	assert.NotNil(t, spec.Volumes[0].EmptyDir)
}

// External identity: there is no realm to prepare an account in. The run reads
// whatever Secret the admin provided, and nothing in the chart makes one up.
func TestVerifyJobWithExternalIdentity(t *testing.T) {
	z := base("zaentrum-beta")
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.Identity.Issuer = "https://sso.example.org/realms/x"
	z.Spec.Hostname = "media.example.org"
	yes := true
	z.Spec.Routing.ProvisionRoutes = &yes

	job := verifyJob(t, z)
	spec := job.Spec.Template.Spec
	assert.Empty(t, spec.InitContainers)
	assert.Empty(t, spec.Volumes)
	env := envByName(spec.Containers[0])
	secretRef(t, env, "ZAE_DOCTOR_USER", VerifySecretName, "username")
	secretRef(t, env, "ZAE_DOCTOR_PASSWORD", VerifySecretName, "password")
	assert.Contains(t, spec.Containers[0].Args, "https://media.example.org", "Routes terminate TLS")

	_, tests := renderSplit(t, z)
	assert.Nil(t, find(t, tests, "Secret", VerifySecretName), "no made-up account for an external provider")
}

// The account Secret the chart renders is for a plain `helm test` of a
// self-host install only — never with external secrets or identity — and it is
// a hook, never a platform object.
func TestVerifyAccountHookSecret(t *testing.T) {
	platform, tests := renderSplit(t, base("zaentrum"))
	sec := find(t, tests, "Secret", VerifySecretName)
	require.NotNil(t, sec)
	assert.Equal(t, "test", sec.GetAnnotations()["helm.sh/hook"])
	user, _, _ := unstructured.NestedString(sec.Object, "stringData", "username")
	assert.Equal(t, VerifyAccountName, user)
	pw, _, _ := unstructured.NestedString(sec.Object, "stringData", "password")
	assert.Len(t, pw, 32)
	assert.Nil(t, find(t, platform, "Secret", VerifySecretName))

	_, tests = renderSplit(t, demoCR("zaentrum-demo"))
	assert.Nil(t, find(t, tests, "Secret", VerifySecretName), "secrets.external: no rendered secrets at all")
}

// The doctor is pointed where users arrive: https wherever the edge terminates
// TLS, http on a plain self-host Ingress.
func TestVerifyJobPublicURL(t *testing.T) {
	yes := true
	cases := map[string]struct {
		mutate func(*zaentrumv1alpha1.Zaentrum)
		want   string
	}{
		"self-host ingress": {func(*zaentrumv1alpha1.Zaentrum) {}, "http://zaentrum.localhost"},
		"https issuer behind a proxy": {func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Identity.IssuerScheme = "https"
		}, "https://zaentrum.localhost"},
		"OpenShift Routes": {func(z *zaentrumv1alpha1.Zaentrum) {
			z.Spec.Routing.ProvisionRoutes = &yes
		}, "https://zaentrum.localhost"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			z := base("zaentrum")
			tc.mutate(z)
			job := verifyJob(t, z)
			assert.Equal(t, tc.want, job.Spec.Template.Spec.Containers[0].Args[2])
		})
	}
}

// spec.verification.enabled=false renders no checks at all.
func TestVerificationDisabledRendersNoHook(t *testing.T) {
	z := base("zaentrum")
	no := false
	z.Spec.Verification.Enabled = &no
	platform, tests := renderSplit(t, z)
	assert.Empty(t, tests)
	assert.Nil(t, VerifyJob(tests))
	assert.NotEmpty(t, platform)
}

// Nothing in the rendered Job carries a credential: every secret is a
// reference, so the Job object itself — readable by anyone who can list Jobs —
// holds none.
func TestVerifyJobCarriesNoCredential(t *testing.T) {
	_, tests := renderSplit(t, base("zaentrum"))
	job := VerifyJob(tests)
	require.NotNil(t, job)
	blob := fmt.Sprintf("%v", job.Object)
	for _, secret := range []string{"dev-change-me", "zaentrum-manager-dev-change-me"} {
		assert.NotContains(t, blob, secret)
	}
	hook := find(t, tests, "Secret", VerifySecretName)
	require.NotNil(t, hook)
	pw, _, _ := unstructured.NestedString(hook.Object, "stringData", "password")
	assert.False(t, strings.Contains(blob, pw), "the Job must not inline the account's password")
}

// The verification Job is found by name among the hooks, not by being the
// first Job: an addon-like chart test beside it must not be taken for it.
func TestVerifyJobPicksTheVerificationJob(t *testing.T) {
	_, tests := renderSplit(t, base("zaentrum"))
	want := VerifyJob(tests)
	require.NotNil(t, want)
	other := want.DeepCopy()
	other.SetName("zaentrum-other-test")

	assert.Same(t, want, VerifyJob(append([]*unstructured.Unstructured{other}, tests...)))
	assert.Nil(t, VerifyJob([]*unstructured.Unstructured{other}))
	secret := &unstructured.Unstructured{Object: map[string]interface{}{"kind": "Secret"}}
	secret.SetName(VerifyJobName)
	assert.Nil(t, VerifyJob([]*unstructured.Unstructured{secret}), "the hook Secret shares the name")
}
