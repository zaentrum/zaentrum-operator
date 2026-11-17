package controller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// The record the ConfigMap zaentrum-backup holds for every set's backup.json:
// the operator's version, the resource's spec as the API server keeps it, the
// names of the Secrets it refers to and of the provided ones that exist, never
// a value, and the platform's addons.
func TestRC10TheRecordSaysWhatMadeTheSets(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	z.Spec.Mail = zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "zaentrum@example.org", User: "zaentrum", SecretName: "mail-password"}
	z.Spec.EventStreaming.CertSecret = "kafka-user"
	z.Spec.ImagePullSecrets = []string{"registry", "mail-password"}
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace},
			Data: map[string][]byte{"api-key": []byte("a value never named")}}
	}
	addon := func(name, ns, ref, version string) *zaentrumv1alpha1.ZaentrumAddon {
		return &zaentrumv1alpha1.ZaentrumAddon{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: zaentrumv1alpha1.ZaentrumAddonSpec{Chart: zaentrumv1alpha1.AddonChart{Ref: ref, Version: version}}}
	}
	s := selfScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z, secret("katalog-tmdb"), secret("zaentrum-db"), secret("zaentrum-keycloak"),
		addon("tv-guide", z.Namespace, "oci://registry.example.org/charts/tv-guide", "1.2.0"),
		addon("arrivals", z.Namespace, "https://charts.example.org/arrivals-0.3.0.tgz", ""),
		addon("elsewhere", "other", "oci://registry.example.org/charts/elsewhere", "9.9.9")).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}

	rec := r.backupRecord(context.Background(), z, "v0.5.1")
	assert.Equal(t, "v0.5.1", rec.Operator)
	assert.Equal(t, []string{"kafka-user", "katalog-tmdb", "mail-password", "registry", "zaentrum-tls"}, rec.Secrets,
		"what the resource names, the certificate's by its default, and the metadata key, which exists; "+
			"neither the platform's own Secrets nor a provided one that is not there")
	assert.Equal(t, []templates.BackupAddon{
		{Name: "arrivals", Chart: "https://charts.example.org/arrivals-0.3.0.tgz"},
		{Name: "tv-guide", Chart: "oci://registry.example.org/charts/tv-guide", Version: "1.2.0"},
	}, rec.Addons, "the platform's own, by name")

	var spec map[string]any
	require.NoError(t, json.Unmarshal([]byte(rec.Resource), &spec))
	assert.Equal(t, "v0.5.0", spec["version"])
	assert.Equal(t, "media.example.org", spec["hostname"])
	mail, _ := spec["mail"].(map[string]any)
	assert.Equal(t, "mail-password", mail["secretName"])
	assert.NotContains(t, rec.Resource, "a value never named")

	t.Run("R.C10 with external databases nothing is backed up, and nothing more is read", func(t *testing.T) {
		z := z.DeepCopy()
		z.Spec.Databases.Mode = "external"
		assert.Equal(t, templates.BackupRecord{Operator: "v0.5.1"}, r.backupRecord(context.Background(), z, "v0.5.1"))
	})
}

// providedSecrets are the Secrets the chart reads by a fixed name that the
// operator does not make: every optional reference to a literal name in the
// rendered platform, but the platform's own and the check's account.
func TestRC10TheProvidedSecretsAreThoseTheChartReadsAndNobodyMakes(t *testing.T) {
	own := map[string]bool{templates.VerifySecretName: true}
	for _, ps := range platformSecrets {
		own[ps.name] = true
	}
	found := map[string]bool{}
	for _, z := range []*zaentrumv1alpha1.Zaentrum{verifyCR(), externalIdentityCR()} {
		z.Spec.Features.Pipeline = true
		z.Spec.LiveWorker.Enabled = true
		v := templates.NewValues(z)
		objs, err := templates.Render(v)
		require.NoError(t, err)
		for _, o := range objs {
			for _, ref := range secretKeyRefs(o) {
				if ref.Optional != nil && *ref.Optional && !own[ref.Name] && ref.Name != z.Spec.Mail.SecretName {
					found[ref.Name] = true
				}
			}
		}
	}
	var names []string
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	assert.Equal(t, providedSecrets, names)
}

// externalIdentityCR is a platform signing in with an identity provider of its
// own, whose workers' client is a Secret the admin provides.
func externalIdentityCR() *zaentrumv1alpha1.Zaentrum {
	z := verifyCR()
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.Identity.Issuer = "https://id.example.org/realms/home"
	return z
}

// secretKeyRefs are the secretKeyRefs of every container of a rendered
// workload.
func secretKeyRefs(o *unstructured.Unstructured) []corev1.SecretKeySelector {
	var spec corev1.PodSpec
	tpl, found, _ := unstructured.NestedMap(o.Object, "spec", "template", "spec")
	if !found {
		tpl, found, _ = unstructured.NestedMap(o.Object, "spec", "jobTemplate", "spec", "template", "spec")
	}
	if !found {
		return nil
	}
	raw, _ := json.Marshal(tpl)
	_ = json.Unmarshal(raw, &spec)
	var out []corev1.SecretKeySelector
	for _, cs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, ct := range cs {
			for _, e := range ct.Env {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					out = append(out, *e.ValueFrom.SecretKeyRef)
				}
			}
		}
	}
	return out
}

// Through Reconcile: the ConfigMap zaentrum-backup the platform is applied
// with holds this pass's record, the operator's version as status.controller
// has it.
func TestRC10ReconcileKeepsTheRecordInTheConfigMap(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	z.Spec.TLS = &zaentrumv1alpha1.TLSSpec{SecretName: "home-tls"}
	tmdb := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "katalog-tmdb", Namespace: z.Namespace}}
	tv := &zaentrumv1alpha1.ZaentrumAddon{ObjectMeta: metav1.ObjectMeta{Name: "tv-guide", Namespace: z.Namespace},
		Spec: zaentrumv1alpha1.ZaentrumAddonSpec{Chart: zaentrumv1alpha1.AddonChart{Ref: "oci://registry.example.org/charts/tv-guide", Version: "1.2.0"}}}
	e := newPlatformEnv(t, z, nil, tmdb, tv)
	e.reconcile()

	var cm corev1.ConfigMap
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKey{Namespace: z.Namespace, Name: templates.BackupRecordName}, &cm))
	var p map[string]string
	require.NoError(t, json.Unmarshal([]byte(cm.Data["platform"]), &p))
	got := e.zaentrum()
	require.NotNil(t, got.Status.Controller)
	assert.Equal(t, map[string]string{"name": "zaentrum", "uid": "z-uid", "version": "v0.5.0", "operator": got.Status.Controller.Version}, p)
	assert.JSONEq(t, `["home-tls","katalog-tmdb"]`, cm.Data["secrets"])
	assert.JSONEq(t, `[{"name":"tv-guide","chart":"oci://registry.example.org/charts/tv-guide","version":"1.2.0"}]`, cm.Data["addons"])
	var spec map[string]any
	require.NoError(t, json.Unmarshal([]byte(cm.Data["resource"]), &spec))
	assert.Equal(t, "v0.5.0", spec["version"])
	assert.Equal(t, "media.example.org", spec["hostname"])
}

// With spec.secrets.external the admin provides every one of the platform's
// Secrets, so a set names them all, as the platform reads them; never one the
// operator makes itself.
func TestRC10WithExternalSecretsEveryOneOfThePlatformsIsNamed(t *testing.T) {
	z := verifyCR()
	z.Spec.Secrets.External = true
	z.Spec.LiveWorker.Enabled = true
	s := selfScheme(t)
	r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(z).Build(), Scheme: s}
	rec := r.backupRecord(context.Background(), z, "v0.5.1")
	assert.Equal(t, []string{"zaentrum-db", "zaentrum-demo-user", "zaentrum-keycloak", "zaentrum-keycloak-admin",
		"zaentrum-live-worker", "zaentrum-people", "zaentrum-stream-signing"}, rec.Secrets)
	assert.NotContains(t, rec.Secrets, templates.VerifySecretName, "the check's account is the operator's own")

	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	z.Spec.LiveWorker.Enabled = false
	assert.Equal(t, []string{"zaentrum-db", "zaentrum-stream-signing"}, r.backupRecord(context.Background(), z, "v0.5.1").Secrets,
		"only those the platform reads")

	z.Spec.Secrets.External = false
	assert.Empty(t, r.backupRecord(context.Background(), z, "v0.5.1").Secrets, "the operator makes them: none is named")
}

// An addon's version is the version installed, from its status, else the one
// its resource asks for.
func TestRC10AnAddonsVersionIsTheOneInstalled(t *testing.T) {
	a := &zaentrumv1alpha1.ZaentrumAddon{Spec: zaentrumv1alpha1.ZaentrumAddonSpec{
		Chart: zaentrumv1alpha1.AddonChart{Ref: "https://charts.example.org/arrivals-0.3.0.tgz", Version: "0.2.0"}}}
	assert.Equal(t, "0.2.0", installedVersion(a), "nothing installed: the one asked for")
	a.Status.LastAppliedChart = &zaentrumv1alpha1.AddonChart{Ref: a.Spec.Chart.Ref, Version: "0.1.0", Digest: "sha256:aaa"}
	assert.Equal(t, "0.1.0", installedVersion(a), "the version applied")
	a.Status.Plan = &zaentrumv1alpha1.AddonPlan{Chart: zaentrumv1alpha1.AddonChartInfo{Name: "arrivals", Version: "0.3.0", Digest: "sha256:aaa"}}
	assert.Equal(t, "0.3.0", installedVersion(a), "the applied archive's own version")
	a.Status.Plan.Chart.Digest = "sha256:bbb"
	assert.Equal(t, "0.1.0", installedVersion(a), "a plan of another archive is not installed")
	a.Status.LastAppliedChart.Version = ""
	assert.Equal(t, "0.2.0", installedVersion(a))

	// The record takes it so.
	z := verifyCR()
	installed := &zaentrumv1alpha1.ZaentrumAddon{ObjectMeta: metav1.ObjectMeta{Name: "arrivals", Namespace: z.Namespace},
		Spec: zaentrumv1alpha1.ZaentrumAddonSpec{Chart: zaentrumv1alpha1.AddonChart{Ref: "oci://registry.example.org/charts/arrivals", Version: "0.4.0"}},
		Status: zaentrumv1alpha1.ZaentrumAddonStatus{LastAppliedChart: &zaentrumv1alpha1.AddonChart{
			Ref: "oci://registry.example.org/charts/arrivals", Version: "0.3.0", Digest: "sha256:ccc"}}}
	s := selfScheme(t)
	r := &ZaentrumReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(z, installed).Build(), Scheme: s}
	assert.Equal(t, []templates.BackupAddon{{Name: "arrivals", Chart: "oci://registry.example.org/charts/arrivals", Version: "0.3.0"}},
		r.backupRecord(context.Background(), z, "v0.5.1").Addons, "installed, not asked for")
}

// Through Reconcile: every URL in the record, an addon's chart or one in the
// resource's spec, is written without its user, its password and its query;
// a name stays a name.
func TestRC10AURLIsWrittenWithoutItsUserItsPasswordAndItsQuery(t *testing.T) {
	z := verifyCR()
	z.Spec.Version = "v0.5.0"
	z.Spec.Identity.Issuer = "https://someone:pa55@id.example.org/realms/home?prompt=login#top"
	z.Spec.EventStreaming.Bootstrap = "SASL_SSL://someone:pa55@k1.example.org:9093?x=1,k2.example.org:9093"
	z.Spec.Mail = zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "zaentrum@example.org", User: "someone@example.org", SecretName: "mail-password"}
	tv := &zaentrumv1alpha1.ZaentrumAddon{ObjectMeta: metav1.ObjectMeta{Name: "tv-guide", Namespace: z.Namespace},
		Spec: zaentrumv1alpha1.ZaentrumAddonSpec{Chart: zaentrumv1alpha1.AddonChart{
			Ref: "oci://someone:pa55@registry.example.org/charts/tv-guide?ticket=abc", Version: "1.2.0"}}}
	e := newPlatformEnv(t, z, nil, tv)
	e.reconcile()

	var cm corev1.ConfigMap
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKey{Namespace: z.Namespace, Name: templates.BackupRecordName}, &cm))
	for key, value := range cm.Data {
		assert.NotContains(t, value, "pa55", key)
		assert.NotContains(t, value, "ticket=", key)
		assert.NotContains(t, value, "prompt=", key)
	}
	assert.JSONEq(t, `[{"name":"tv-guide","chart":"oci://registry.example.org/charts/tv-guide","version":"1.2.0"}]`, cm.Data["addons"])
	var spec struct {
		Identity       struct{ Issuer string }           `json:"identity"`
		EventStreaming struct{ Bootstrap string }        `json:"eventStreaming"`
		Mail           struct{ User, SecretName string } `json:"mail"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data["resource"]), &spec))
	assert.Equal(t, "https://id.example.org/realms/home#top", spec.Identity.Issuer)
	assert.Equal(t, "SASL_SSL://k1.example.org:9093,k2.example.org:9093", spec.EventStreaming.Bootstrap)
	assert.Equal(t, "someone@example.org", spec.Mail.User, "a name, not a URL")
	assert.Equal(t, "mail-password", spec.Mail.SecretName)
}
