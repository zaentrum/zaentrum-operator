package controller

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// secretsEnv is a reconciler over a fake API server holding z and objs.
func secretsEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum, funcs *interceptor.Funcs, objs ...client.Object) (*ZaentrumReconciler, client.Client) {
	t.Helper()
	s := selfScheme(t)
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, z)...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	return &ZaentrumReconciler{Client: c, Scheme: s}, c
}

func liveSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: name}, &sec); err != nil {
		return nil
	}
	return &sec
}

func secretsCond(z *zaentrumv1alpha1.Zaentrum) *metav1.Condition {
	return meta.FindStatusCondition(z.Status.Conditions, condTypeSecretsGenerated)
}

var bundledSecrets = []string{"zaentrum-db", "zaentrum-stream-signing", "zaentrum-keycloak", "zaentrum-keycloak-admin", "zaentrum-demo-user", peopleSecretName}

func isAlnum(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(strings.Join(alnum, ""), r) {
			return false
		}
	}
	return s != ""
}

// Every Secret the bundled platform reads is made, once, with values nobody
// else has: owned by the Zaentrum, marked as generated, and never touched
// again by a reconcile.
func TestSecretsAreGeneratedOnce(t *testing.T) {
	z := verifyCR()
	r, c := secretsEnv(t, z, nil)
	ctx := context.Background()

	require.NoError(t, r.ensureSecrets(ctx, z))
	first := map[string]map[string][]byte{}
	for _, name := range bundledSecrets {
		sec := liveSecret(t, c, name)
		require.NotNil(t, sec, name)
		assert.True(t, metav1.IsControlledBy(sec, z), "%s goes with the platform", name)
		assert.Equal(t, "true", sec.Labels[labelGenerated], name)
		assert.Equal(t, corev1.SecretTypeOpaque, sec.Type, name)
		first[name] = sec.Data
	}

	db := first["zaentrum-db"]
	assert.Equal(t, "zaentrum", string(db["user"]), "the user the bundled Postgres and its probe know")
	assert.Len(t, db["password"], 32)
	assert.True(t, isAlnum(string(db["password"])), "a database password travels unescaped in a URL")

	key, err := base64.StdEncoding.DecodeString(string(first["zaentrum-stream-signing"]["key"]))
	require.NoError(t, err, "chino-api, chino-stream and katalog-manager read the key as base64")
	assert.Len(t, key, 32)

	assert.Len(t, first["zaentrum-keycloak"]["client-secret"], 32)
	admin := first[adminSecretName]
	assert.Equal(t, firstAdminUsername, string(admin[adminUsernameKey]))
	assert.Len(t, admin[adminPasswordKey], 32)
	assert.Len(t, admin[firstAdminPasswordKey], 24)
	assert.NotEqual(t, admin[adminPasswordKey], admin[firstAdminPasswordKey], "two accounts, two passwords")
	assert.Len(t, first["zaentrum-demo-user"]["password"], 32,
		"without it the realm's demo user would sign in with the literal ${DEMO_USER_PASSWORD}")
	people := first[peopleSecretName]
	assert.Len(t, people["client-secret"], 32, "the zaentrum-people client's, which the realm Job sets")
	assert.Len(t, people["deletion-token"], 43, "256 bits: what chino-api and portal-api show each other to delete an account")
	assert.NotEqual(t, people["client-secret"], people["deletion-token"])
	for name, data := range first {
		for k, v := range data {
			if k != "key" {
				assert.True(t, isAlnum(string(v)), "%s/%s must survive a URL, JSON and a shell unescaped", name, k)
			}
		}
	}

	cond := secretsCond(z)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Generated", cond.Reason)
	assert.Contains(t, cond.Message,
		"kubectl -n zaentrum get secret zaentrum-keycloak-admin -o jsonpath='{.data.realm-admin-password}' | base64 -d",
		"the condition says where the first administrator's password is")
	for name, data := range first {
		for k, v := range data {
			if k != "user" && k != adminUsernameKey {
				assert.NotContains(t, cond.Message, string(v), "status shows where %s/%s is, never what", name, k)
			}
		}
	}

	// Every later pass keeps what is there.
	for i := 0; i < 3; i++ {
		require.NoError(t, r.ensureSecrets(ctx, z))
	}
	for _, name := range bundledSecrets {
		assert.Equal(t, first[name], liveSecret(t, c, name).Data, "%s is never rotated", name)
	}
}

// Two platforms, two sets of values: nothing is derived from a constant.
func TestSecretsDifferPerInstall(t *testing.T) {
	values := map[string]bool{}
	for i := 0; i < 2; i++ {
		z := verifyCR()
		r, c := secretsEnv(t, z, nil)
		require.NoError(t, r.ensureSecrets(context.Background(), z))
		for _, name := range bundledSecrets {
			for k, v := range liveSecret(t, c, name).Data {
				if k == "user" || k == adminUsernameKey {
					continue
				}
				assert.False(t, values[string(v)], "%s/%s repeats a value of another install", name, k)
				values[string(v)] = true
			}
		}
	}
}

// A key that went missing is filled in; the keys that are there stay as they
// are, whoever made the Secret.
func TestSecretsFillAMissingKey(t *testing.T) {
	z := verifyCR()
	theirs := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: verifyNS},
		Data:       map[string][]byte{adminUsernameKey: []byte("root"), adminPasswordKey: []byte("someone-elses-Password1")},
	}
	r, c := secretsEnv(t, z, nil, theirs)
	require.NoError(t, r.ensureSecrets(context.Background(), z))

	sec := liveSecret(t, c, adminSecretName)
	assert.Equal(t, "root", string(sec.Data[adminUsernameKey]))
	assert.Equal(t, "someone-elses-Password1", string(sec.Data[adminPasswordKey]))
	assert.Len(t, sec.Data[firstAdminPasswordKey], 24, "the missing key is filled in")
	assert.Empty(t, sec.OwnerReferences, "a Secret the operator did not make is not taken over")
	assert.Equal(t, "Provided", secretsCond(z).Reason)
}

// External secrets are someone else's: nothing is made, nothing is read into
// status, and no condition claims otherwise.
func TestSecretsExternalAreLeftAlone(t *testing.T) {
	z := verifyCR()
	z.Spec.Secrets.External = true
	z.Status.Conditions = []metav1.Condition{{Type: condTypeSecretsGenerated, Status: metav1.ConditionTrue, Reason: "Generated"}}
	ci := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-db", Namespace: verifyNS, ResourceVersion: "7"},
		Data:       map[string][]byte{"user": []byte("zaentrum")},
	}
	writes := 0
	r, c := secretsEnv(t, z, &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return cl.Update(ctx, obj, opts...)
		},
	}, ci)

	require.NoError(t, r.ensureSecrets(context.Background(), z))
	assert.Zero(t, writes)
	assert.Nil(t, secretsCond(z))
	for _, name := range bundledSecrets[1:] {
		assert.Nil(t, liveSecret(t, c, name), name)
	}
	assert.Equal(t, map[string][]byte{"user": []byte("zaentrum")}, liveSecret(t, c, "zaentrum-db").Data,
		"a key missing from a Secret CI made is CI's to add")
}

// External identity has no Keycloak: only the database and the signing key.
func TestSecretsWithExternalIdentity(t *testing.T) {
	z := verifyCR()
	z.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	r, c := secretsEnv(t, z, nil)
	require.NoError(t, r.ensureSecrets(context.Background(), z))

	assert.NotNil(t, liveSecret(t, c, "zaentrum-db"))
	assert.NotNil(t, liveSecret(t, c, "zaentrum-stream-signing"))
	for _, name := range []string{"zaentrum-keycloak", adminSecretName, "zaentrum-demo-user", peopleSecretName} {
		assert.Nil(t, liveSecret(t, c, name), "%s belongs to the bundled Keycloak", name)
	}
	cond := secretsCond(z)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "the operator keeps zaentrum-db, zaentrum-stream-signing", cond.Message)
}

// An install made from an earlier chart keeps the values that chart shipped to
// everyone. They are named in status — never replaced, since each also lives
// in the database, the realm or Keycloak's admin user.
func TestSecretsPublishedDefaultsAreReportedNotReplaced(t *testing.T) {
	z := verifyCR()
	old := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-db", Namespace: verifyNS},
			Data: map[string][]byte{"user": []byte("zaentrum"), "password": []byte("zaentrum-dev-change-me")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-stream-signing", Namespace: verifyNS},
			Data: map[string][]byte{"key": []byte("ZGV2LW9ubHktY2hhbmdlLW1lLXN0cmVhbS1zaWduaW5nLWtleQ==")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-keycloak", Namespace: verifyNS},
			Data: map[string][]byte{"client-secret": []byte("zaentrum-manager-dev-change-me")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: verifyNS},
			Data: map[string][]byte{adminUsernameKey: []byte("admin"), adminPasswordKey: []byte("dev-change-me"), firstAdminPasswordKey: []byte("dev-change-me")}},
	}
	r, c := secretsEnv(t, z, nil, old...)
	require.NoError(t, r.ensureSecrets(context.Background(), z))

	for _, o := range old {
		was := o.(*corev1.Secret)
		assert.Equal(t, was.Data, liveSecret(t, c, was.Name).Data, "%s keeps its values", was.Name)
	}
	cond := secretsCond(z)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "PublishedDefaults", cond.Reason)
	for _, want := range []string{"zaentrum-db (password)", "zaentrum-stream-signing (key)", "zaentrum-keycloak (client-secret)",
		"zaentrum-keycloak-admin (password, realm-admin-password)"} {
		assert.Contains(t, cond.Message, want)
	}
	assert.NotContains(t, cond.Message, "dev-change-me", "status names the Secrets, not the values")
	assert.NotContains(t, cond.Message, "zaentrum-demo-user", "made just now, it holds nothing published")
	assert.NotNil(t, liveSecret(t, c, "zaentrum-demo-user"))

	// The operator's own templates before the chart shipped other values.
	for _, v := range []string{"stube-dev-change-me", "dev"} {
		z := verifyCR()
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "zaentrum-db", Namespace: verifyNS},
			Data: map[string][]byte{"user": []byte("zaentrum"), "password": []byte(v)}}
		if v == "dev" {
			sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: verifyNS},
				Data: map[string][]byte{adminUsernameKey: []byte("admin"), adminPasswordKey: []byte("dev"), firstAdminPasswordKey: []byte("dev")}}
		}
		r, _ := secretsEnv(t, z, nil, sec)
		require.NoError(t, r.ensureSecrets(context.Background(), z))
		assert.Equal(t, "PublishedDefaults", secretsCond(z).Reason, v)
	}
}

// A Secret that cannot be made fails the reconcile, before anything that reads
// it is applied, and says which.
func TestSecretsThatCannotBeMadeFailTheReconcile(t *testing.T) {
	z := verifyCR()
	s := selfScheme(t)
	applied := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return assert.AnError
				}
				return cl.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				applied++
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
		}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)
	assert.Zero(t, applied, "nothing is applied before its Secrets exist")
	var got zaentrumv1alpha1.Zaentrum
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &got))
	assert.Equal(t, "Error", got.Status.Phase)
	cond := secretsCond(&got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Error", cond.Reason)
	assert.Contains(t, cond.Message, "zaentrum-db")
}

// Through Reconcile: the Secrets exist before the first workload is applied,
// and the chart's render carries none of them.
func TestReconcileMakesTheSecretsFirst(t *testing.T) {
	z := verifyCR()
	s := selfScheme(t)
	var order []string
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(z).
		WithStatusSubresource(&zaentrumv1alpha1.Zaentrum{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					order = append(order, "generate "+obj.GetName())
				}
				return cl.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				order = append(order, "apply "+obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
				return applyAsCreateOrUpdate(ctx, cl, obj, patch, opts...)
			},
		}).Build()
	r := &ZaentrumReconciler{Client: c, Scheme: s}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: verifyNS, Name: "zaentrum"}})
	require.NoError(t, err)

	require.Greater(t, len(order), len(bundledSecrets))
	for i, name := range bundledSecrets {
		assert.Equal(t, "generate "+name, order[i])
	}
	for _, o := range order[len(bundledSecrets):] {
		assert.False(t, strings.HasPrefix(o, "apply Secret/"), "the render applied a Secret: %s", o)
		assert.False(t, strings.HasPrefix(o, "generate "), "a Secret made after the platform was applied: %s", o)
	}
}
