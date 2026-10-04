package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// This file keeps the platform's own Secrets when nobody else does.
//
// With spec.secrets.external they are someone else's — CI makes them — and the
// operator leaves them alone. Otherwise the operator makes each Secret the
// platform needs, once: every value from crypto/rand, the Secret owned by the
// Zaentrum so it goes with the platform, and nothing in it ever rotated by a
// reconcile. A key that has gone missing is filled in; a value that is there
// is never replaced, since each one also lives where the Secret does not reach
// — the database's role, the realm's client, Keycloak's admin user — and a new
// value would only lock the platform out of itself.
//
// The chart renders none of them for the operator (templates.go renders it with
// secrets.external): a random value in a render the operator applies every 30
// seconds would rotate on every pass, and a fixed one is the same for every
// install that ever used it. A plain Helm install makes its own
// (templates/secrets.yaml).
//
// What the SecretsGenerated condition says: where the first administrator's
// one-time password is, and — on an install made from an earlier chart, which
// shipped the same values to everyone — which Secrets still hold them. Those
// are reported, never replaced: rotating one means changing it where it lives
// too, which is for a person to do (operator/README.md).

const (
	condTypeSecretsGenerated = "SecretsGenerated"

	// labelGenerated marks a Secret the operator made.
	labelGenerated = "zaentrum.io/generated"

	// The Secret and key holding the first administrator's one-time password:
	// the realm import's ${ZAENTRUM_ADMIN_PASSWORD}, temporary, so Keycloak asks
	// for a new one at the first sign-in.
	adminSecretName       = "zaentrum-keycloak-admin"
	adminPasswordKey      = "password"
	adminUsernameKey      = "username"
	firstAdminPasswordKey = "realm-admin-password"
	firstAdminUsername    = "admin"

	// peopleSecretName holds the People page's credentials: the
	// zaentrum-people client's secret, and the token account deletion is
	// called with between chino-api and portal-api.
	peopleSecretName = "zaentrum-people"
)

// alnum is what a generated value is made of: it travels unescaped through a
// database URL, a realm import's JSON, a form body and a shell.
var alnum = []string{
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"abcdefghijklmnopqrstuvwxyz",
	"0123456789",
}

// secretKey is one key of a platform Secret: how to make its value, and the
// values an earlier chart shipped to every install.
type secretKey struct {
	name      string
	make      func() ([]byte, error)
	published []string
}

// platformSecret is a Secret the platform's workloads read.
type platformSecret struct {
	name string
	// bundled marks a Secret only the bundled Keycloak needs.
	bundled bool
	keys    []secretKey
}

func fixed(v string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(v), nil }
}

func random(n int) func() ([]byte, error) {
	return func() ([]byte, error) { return randomString(alnum, n) }
}

// signingKey is 32 random bytes, base64 as chino-api, chino-stream and
// katalog-manager read STREAM_SIGNING_KEY.
func signingKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(key)), nil
}

// platformSecrets are the Secrets the chart's workloads reference by name.
// The published values are those of the chart's templates/secrets.yaml, of
// deploy/base and of the operator's templates before the chart.
var platformSecrets = []platformSecret{
	{name: "zaentrum-db", keys: []secretKey{
		{name: "user", make: fixed("zaentrum")},
		{name: "password", make: random(32), published: []string{"zaentrum-dev-change-me", "stube-dev-change-me"}},
	}},
	{name: "zaentrum-stream-signing", keys: []secretKey{
		{name: "key", make: signingKey, published: []string{
			"ZGV2LW9ubHktY2hhbmdlLW1lLXN0cmVhbS1zaWduaW5nLWtleQ==",
			"ZGV2LW9ubHktY2hhbmdlLW1lLW5vdC1mb3ItcHJvZCE=",
		}},
	}},
	// The zaentrum-manager client's secret: the realm import's
	// ${ZAENTRUM_MANAGER_SECRET}, and what katalog-manager-api and the pipeline
	// workers sign in with. Its service account manages the realm's users.
	{name: "zaentrum-keycloak", bundled: true, keys: []secretKey{
		{name: "client-secret", make: random(32), published: []string{"zaentrum-manager-dev-change-me", "stube-manager-dev-change-me"}},
	}},
	// The master realm's bootstrap admin, which the operator's Jobs sign in to
	// the admin API with, and the first administrator's one-time password.
	{name: adminSecretName, bundled: true, keys: []secretKey{
		{name: adminUsernameKey, make: fixed(firstAdminUsername)},
		{name: adminPasswordKey, make: random(32), published: []string{"dev-change-me", "dev"}},
		{name: firstAdminPasswordKey, make: random(24), published: []string{"dev-change-me", "dev"}},
	}},
	// The realm import's ${DEMO_USER_PASSWORD}. Keycloak leaves a placeholder
	// it cannot resolve as it is, so without this Secret the realm's demo user
	// would sign in with the password "${DEMO_USER_PASSWORD}".
	{name: "zaentrum-demo-user", bundled: true, keys: []secretKey{
		{name: "password", make: random(32)},
	}},
	// The People page's. client-secret is the zaentrum-people client's, which
	// the realm Job sets in the realm and portal-api signs in with to manage
	// the realm's people — that client may view, query and manage users,
	// nothing else. deletion-token is what chino-api shows portal-api when a
	// person deletes their own account, and portal-api shows chino-api when an
	// admin deletes someone's: the call comes from the service that deletes
	// that person's data, and from no client.
	{name: peopleSecretName, bundled: true, keys: []secretKey{
		{name: "client-secret", make: random(32)},
		{name: "deletion-token", make: random(43)},
	}},
}

// ensureSecrets makes the platform Secrets that are missing, fills in their
// missing keys, and writes the SecretsGenerated condition. It runs before the
// platform is applied, so no pod starts without the Secrets it reads.
func (r *ZaentrumReconciler) ensureSecrets(ctx context.Context, z *zaentrumv1alpha1.Zaentrum) error {
	if z.Spec.Secrets.External {
		meta.RemoveStatusCondition(&z.Status.Conditions, condTypeSecretsGenerated)
		return nil
	}
	var names, published []string
	provided := false
	for _, ps := range platformSecrets {
		if ps.bundled && !bundledIdentity(z) {
			continue
		}
		names = append(names, ps.name)
		keys, generated, err := r.ensureSecret(ctx, z, ps)
		provided = provided || !generated
		if err != nil {
			setCondition(z, condTypeSecretsGenerated, metav1.ConditionFalse, "Error",
				fmt.Sprintf("could not keep Secret %s: %v", ps.name, err))
			return fmt.Errorf("secret %s: %w", ps.name, err)
		}
		if len(keys) > 0 {
			published = append(published, fmt.Sprintf("%s (%s)", ps.name, strings.Join(keys, ", ")))
		}
	}
	if len(published) > 0 {
		setCondition(z, condTypeSecretsGenerated, metav1.ConditionFalse, "PublishedDefaults",
			"these Secrets hold values an earlier chart shipped to every install: "+strings.Join(published, "; ")+
				" — change each where it is used too, then in the Secret (operator/README.md, \"The platform's Secrets\")")
		return nil
	}
	msg := "the operator keeps " + strings.Join(names, ", ")
	if bundledIdentity(z) {
		msg = fmt.Sprintf("the first administrator, %s, signs in with the one-time password in Secret %s "+
			"and chooses a new one there: kubectl -n %s get secret %s -o jsonpath='{.data.%s}' | base64 -d",
			firstAdminUsername, adminSecretName, z.Namespace, adminSecretName, firstAdminPasswordKey)
	}
	reason := "Generated"
	if provided {
		// Some were there before the operator was asked to keep them.
		reason = "Provided"
	}
	setCondition(z, condTypeSecretsGenerated, metav1.ConditionTrue, reason, msg)
	return nil
}

// ensureSecret makes one platform Secret, or fills in the keys it lacks. It
// returns the keys that hold a published value and whether the operator made
// the Secret. It never changes a value that is there.
func (r *ZaentrumReconciler) ensureSecret(ctx context.Context, z *zaentrumv1alpha1.Zaentrum, ps platformSecret) ([]string, bool, error) {
	key := types.NamespacedName{Namespace: z.Namespace, Name: ps.name}
	var sec corev1.Secret
	err := r.reader().Get(ctx, key, &sec)
	switch {
	case apierrors.IsNotFound(err):
		data := map[string][]byte{}
		for _, k := range ps.keys {
			v, err := k.make()
			if err != nil {
				return nil, false, err
			}
			data[k.name] = v
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ps.name, Namespace: z.Namespace, Labels: map[string]string{
				"app.kubernetes.io/name":      ps.name,
				"app.kubernetes.io/component": "secrets",
				"app.kubernetes.io/part-of":   partOf(z),
				labelGenerated:                "true",
			}},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if err := controllerutil.SetControllerReference(z, &sec, r.Scheme); err != nil {
			return nil, false, err
		}
		if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, false, err
		}
		log.FromContext(ctx).Info("secrets: generated a platform Secret", "secret", ps.name)
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}

	var filled, published []string
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for _, k := range ps.keys {
		v, ok := sec.Data[k.name]
		if !ok || len(v) == 0 {
			// stringData is folded into data by the API server; a Secret read
			// back never carries it.
			nv, err := k.make()
			if err != nil {
				return nil, false, err
			}
			sec.Data[k.name] = nv
			filled = append(filled, k.name)
			continue
		}
		for _, p := range k.published {
			if string(v) == p {
				published = append(published, k.name)
			}
		}
	}
	if len(filled) > 0 {
		if err := r.Update(ctx, &sec); err != nil {
			return nil, false, err
		}
		sort.Strings(filled)
		log.FromContext(ctx).Info("secrets: filled in missing keys", "secret", ps.name, "keys", strings.Join(filled, ","))
	}
	return published, sec.Labels[labelGenerated] == "true", nil
}

// randomString draws n characters from crypto/rand: one of each class first,
// so any password policy that asks for them is met, the rest from all of them,
// then shuffled.
func randomString(classes []string, n int) ([]byte, error) {
	all := strings.Join(classes, "")
	out := make([]byte, n)
	for i := range out {
		set := all
		if i < len(classes) {
			set = classes[i]
		}
		j, err := randIndex(len(set))
		if err != nil {
			return nil, err
		}
		out[i] = set[j]
	}
	for i := len(out) - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return nil, err
		}
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
