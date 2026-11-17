package controller

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/templates"
)

// The platform's mail settings (spec/portal/operations.md, Mail, The
// settings): where they stand, in the Mail condition, and the Secrets of the
// mail server's password that the resource no longer names.

var mailNow = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// mailSecret is a Secret of the mail server's password in the platform's
// namespace, as portal-api makes one: labelled zaentrum.io/mail, with the
// keys data names.
func mailSecret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: verifyNS, UID: types.UID(name + "-uid"),
		Labels: map[string]string{labelMail: "true"}}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

var withPassword = map[string]string{mailPasswordKey: "not-a-real-password"}

// mailCR is a platform whose mail goes through smtp.example.org, signing in as
// user with the password in Secret secret; empty, neither.
func mailCR(user, secret string) *zaentrumv1alpha1.Zaentrum {
	z := verifyCR()
	z.Spec.Mail = zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org", User: user, SecretName: secret}
	return z
}

func mailReconciler(t *testing.T, clock *time.Time, objs ...client.Object) (*ZaentrumReconciler, client.WithWatch) {
	t.Helper()
	s := selfScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &ZaentrumReconciler{Client: c, Scheme: s, Now: func() time.Time { return *clock }}, c
}

// R.C1: the condition Mail says NotSetUp while spec.mail names no server and
// sender, SecretMissing while the Secret it names is not there or lacks its
// key, password, and Configured otherwise: signing in as spec.mail.user with
// that Secret's password, or without signing in. The key counts once it is
// there, empty or not. A user with no Secret named, which the resource's rule
// refuses, is SecretMissing where the rule did not run.
func TestRC1TheMailConditionSaysWhereMailStands(t *testing.T) {
	for name, c := range map[string]struct {
		mail   zaentrumv1alpha1.MailSpec
		secret *corev1.Secret
		status metav1.ConditionStatus
		reason string
		says   string
	}{
		"no mail": {status: metav1.ConditionFalse, reason: "NotSetUp", says: "spec.mail names no server, so the platform sends no mail"},
		"a server and no sender": {mail: zaentrumv1alpha1.MailSpec{Host: "smtp.example.org"},
			status: metav1.ConditionFalse, reason: "NotSetUp", says: "names the server smtp.example.org and no sender"},
		"a sender and no server": {mail: zaentrumv1alpha1.MailSpec{From: "noreply@example.org"},
			status: metav1.ConditionFalse, reason: "NotSetUp", says: "names no server"},
		"no sign-in": {mail: mailCR("", "").Spec.Mail, status: metav1.ConditionTrue, reason: "Configured",
			says: "the platform sends its mail through smtp.example.org (starttls), without signing in"},
		"tls": {mail: zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org", Encryption: "tls"},
			status: metav1.ConditionTrue, reason: "Configured", says: "smtp.example.org (tls)"},
		"a Secret that is not there": {mail: mailCR("mailer", "mail-x7k2p").Spec.Mail, status: metav1.ConditionFalse,
			reason: "SecretMissing", says: "there is no Secret mail-x7k2p: create it with the mail server's password under the key password"},
		"no password": {mail: mailCR("mailer", "mail-x7k2p").Spec.Mail,
			secret: mailSecret("mail-x7k2p", map[string]string{"username": "mailer"}),
			status: metav1.ConditionFalse, reason: "SecretMissing", says: "Secret mail-x7k2p has no key password: put the mail server's password under it"},
		"the password": {mail: mailCR("mailer", "mail-x7k2p").Spec.Mail, secret: mailSecret("mail-x7k2p", withPassword),
			status: metav1.ConditionTrue, reason: "Configured",
			says: "the platform sends its mail through smtp.example.org (starttls), signing in as mailer with the password in Secret mail-x7k2p"},
		"an empty password is a key": {mail: mailCR("mailer", "mail-x7k2p").Spec.Mail,
			secret: mailSecret("mail-x7k2p", map[string]string{mailPasswordKey: ""}), status: metav1.ConditionTrue, reason: "Configured"},
		"a user and no Secret, where the resource's rule did not run": {mail: mailCR("mailer", "").Spec.Mail,
			status: metav1.ConditionFalse, reason: "SecretMissing",
			says: "spec.mail names no Secret for the password of mailer: name it in secretName"},
		"a Secret and no user": {mail: mailCR("", "mail-x7k2p").Spec.Mail, secret: mailSecret("mail-x7k2p", withPassword),
			status: metav1.ConditionTrue, reason: "Configured", says: "without signing in"},
		"a Secret named and not there, and no user": {mail: mailCR("", "mail-x7k2p").Spec.Mail,
			status: metav1.ConditionFalse, reason: "SecretMissing", says: "there is no Secret mail-x7k2p"},
	} {
		t.Run(name, func(t *testing.T) {
			z := verifyCR()
			z.Spec.Mail = c.mail
			objs := []client.Object{}
			if c.secret != nil {
				objs = append(objs, c.secret)
			}
			r, _ := mailReconciler(t, &mailNow, objs...)
			r.reportMail(context.Background(), z)
			got := meta.FindStatusCondition(z.Status.Conditions, condTypeMail)
			require.NotNil(t, got)
			assert.Equal(t, c.status, got.Status, got.Message)
			assert.Equal(t, c.reason, got.Reason, got.Message)
			assert.Contains(t, got.Message, c.says)
			assert.NotContains(t, got.Message, "not-a-real-password", "a password never reaches the status")
		})
	}
}

// R.C1: a Secret labelled zaentrum.io/mail that the resource has not named
// for 10 minutes is removed — one portal-api made and the resource never
// named, one the resource named before another or before its mail settings
// were removed. Never one it names, one another platform of the namespace
// names, one something else owns, one without the label, nor one of another
// namespace; and never sooner than 10 minutes after the operator first saw the
// resource not name it, so the time portal-api takes to name a Secret it just
// made is never cut short.
func TestRC1MailSecretsTheResourceNoLongerNamesAreRemoved(t *testing.T) {
	ctx := context.Background()
	clock := mailNow
	z := mailCR("mailer", "mail-a")
	other := &zaentrumv1alpha1.Zaentrum{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: verifyNS, UID: "other-uid"}}
	other.Spec.Mail.SecretName = "mail-other"
	ownedByOther := mailSecret("mail-owned", withPassword)
	ownedByOther.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "x-uid"}}
	ownedByZ := mailSecret("mail-mine", withPassword)
	ownedByZ.OwnerReferences = []metav1.OwnerReference{{APIVersion: "zaentrum.io/v1alpha1", Kind: "Zaentrum", Name: z.Name, UID: z.UID}}
	plain := mailSecret("mail-plain", withPassword)
	plain.Labels = nil
	elsewhere := mailSecret("mail-elsewhere", withPassword)
	elsewhere.Namespace = "elsewhere"
	r, c := mailReconciler(t, &clock, z, other,
		mailSecret("mail-a", withPassword), mailSecret("mail-old", withPassword), mailSecret("mail-other", withPassword),
		ownedByOther, ownedByZ, plain, elsewhere)
	pass := func() { r.collectMailSecrets(ctx, z) }
	there := func(ns, name string) bool {
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Secret{})
		if apierrors.IsNotFound(err) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	kept := func(when string) {
		t.Helper()
		assert.True(t, there(verifyNS, "mail-a") == (z.Spec.Mail.SecretName == "mail-a"), "%s: mail-a", when)
		assert.True(t, there(verifyNS, "mail-other"), "%s: another platform of the namespace names it", when)
		assert.True(t, there(verifyNS, "mail-owned"), "%s: something else owns it", when)
		assert.True(t, there(verifyNS, "mail-plain"), "%s: not labelled zaentrum.io/mail", when)
		assert.True(t, there("elsewhere", "mail-elsewhere"), "%s: another namespace", when)
	}

	pass()
	assert.True(t, there(verifyNS, "mail-old"), "first seen unnamed: the 10 minutes start")
	assert.True(t, there(verifyNS, "mail-mine"))
	clock = clock.Add(mailSecretGrace - time.Second)
	pass()
	assert.True(t, there(verifyNS, "mail-old"), "not before 10 minutes")
	clock = clock.Add(2 * time.Second)
	pass()
	assert.False(t, there(verifyNS, "mail-old"), "unnamed for 10 minutes: removed")
	assert.False(t, there(verifyNS, "mail-mine"), "the platform's own, unnamed: removed")
	kept("10 minutes on")

	// A new password in a new Secret, which the resource then names: the one
	// it named before goes 10 minutes after it stopped naming it, however old.
	require.NoError(t, c.Create(ctx, mailSecret("mail-b", withPassword)))
	z.Spec.Mail.SecretName = "mail-b"
	pass()
	assert.True(t, there(verifyNS, "mail-a"), "named a moment ago")
	clock = clock.Add(mailSecretGrace - time.Second)
	pass()
	assert.True(t, there(verifyNS, "mail-a"))
	clock = clock.Add(2 * time.Second)
	pass()
	assert.False(t, there(verifyNS, "mail-a"), "not named for 10 minutes")
	assert.True(t, there(verifyNS, "mail-b"), "the one it names")

	// One named again before its time is up starts over once it is not.
	require.NoError(t, c.Create(ctx, mailSecret("mail-c", withPassword)))
	pass()
	clock = clock.Add(5 * time.Minute)
	z.Spec.Mail.SecretName = "mail-c"
	pass()
	clock = clock.Add(6 * time.Minute)
	z.Spec.Mail.SecretName = "mail-b"
	pass()
	assert.True(t, there(verifyNS, "mail-c"), "named until a moment ago")
	clock = clock.Add(mailSecretGrace - time.Second)
	pass()
	assert.True(t, there(verifyNS, "mail-c"))
	clock = clock.Add(2 * time.Second)
	pass()
	assert.False(t, there(verifyNS, "mail-c"))

	// The mail settings removed: the Secret they named goes with them.
	z.Spec.Mail = zaentrumv1alpha1.MailSpec{}
	pass()
	clock = clock.Add(mailSecretGrace + time.Second)
	pass()
	assert.False(t, there(verifyNS, "mail-b"), "no mail settings name it")
	kept("after the settings were removed")

	// An operator that has not seen a Secret unnamed — one just started —
	// counts the 10 minutes from when it does.
	require.NoError(t, c.Create(ctx, mailSecret("mail-d", withPassword)))
	fresh := &ZaentrumReconciler{Client: c, Scheme: r.Scheme, Now: func() time.Time { return clock }}
	fresh.collectMailSecrets(ctx, z)
	clock = clock.Add(mailSecretGrace - time.Second)
	fresh.collectMailSecrets(ctx, z)
	assert.True(t, there(verifyNS, "mail-d"))
	clock = clock.Add(2 * time.Second)
	fresh.collectMailSecrets(ctx, z)
	assert.False(t, there(verifyNS, "mail-d"))
	assert.Empty(t, fresh.mailUnnamed, "nothing is remembered of a Secret that is gone")
}

// R.C1: every pass of the reconciler writes the Mail condition, beside the
// others, and removes the mail Secrets the resource has not named for 10
// minutes; neither moves the phase. It looks for them in the platform's
// namespace by their label, and reads their metadata alone: never a Secret's
// data, so no password but the one named is ever read.
func TestRC1APassSaysWhereMailStandsAndRemovesWhatIsNoLongerNamed(t *testing.T) {
	z := mailCR("mailer", "mail-a")
	var mailLists []client.ListOptions
	fullSecretLists := 0
	e := newPlatformEnv(t, z, &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		var o client.ListOptions
		o.ApplyOptions(opts)
		switch l := list.(type) {
		case *corev1.SecretList:
			fullSecretLists++
		case *metav1.PartialObjectMetadataList:
			if l.GroupVersionKind().Kind == "SecretList" {
				mailLists = append(mailLists, o)
			}
		}
		return c.List(ctx, list, opts...)
	}}, mailSecret("mail-a", withPassword), mailSecret("mail-old", withPassword))
	e.reconcile()
	require.Len(t, mailLists, 1, "one list of the mail Secrets a pass")
	assert.Equal(t, verifyNS, mailLists[0].Namespace)
	require.NotNil(t, mailLists[0].LabelSelector)
	assert.Equal(t, labelMail, mailLists[0].LabelSelector.String(), "by the label alone")
	assert.Zero(t, fullSecretLists, "no Secret's data is listed")
	got := e.zaentrum()
	c := meta.FindStatusCondition(got.Status.Conditions, condTypeMail)
	require.NotNil(t, c)
	assert.Equal(t, "Configured", c.Reason, c.Message)
	assert.Equal(t, "Progressing", got.Status.Phase, "the parts are not ready on a fake API server; mail moves nothing")

	e.clock = e.clock.Add(mailSecretGrace + time.Second)
	e.reconcile()
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "mail-old"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "unnamed for 10 minutes: removed")
	require.NoError(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: verifyNS, Name: "mail-a"}, &corev1.Secret{}))

	require.NoError(t, e.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-a", Namespace: verifyNS}}))
	e.reconcile()
	c = meta.FindStatusCondition(e.zaentrum().Status.Conditions, condTypeMail)
	require.NotNil(t, c)
	assert.Equal(t, "SecretMissing", c.Reason, c.Message)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
}

// R.C1: spec.mail is checked where it is written: the port from 1 to 65535,
// the encryption one of starttls, tls and none, the sender's name at most 60
// characters, at most 10 further addresses, a server needs a sender and a user
// the Secret of its password (CEL rules, which only a cluster runs, so their
// text is compared). The defaults —
// the port by the encryption, starttls, the name zaentrum — are the
// operator's and the chart's, not the schema's.
func TestRC1TheMailSchemaChecksWhatTheSpecSays(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	mail := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties", "mail")
	require.NotNil(t, mail, "spec.mail")
	props, _ := dig(mail, "properties").(map[string]any)
	assert.ElementsMatch(t, []string{"host", "port", "encryption", "from", "fromName", "user", "secretName", "alertsTo"}, keysOf(props))

	for _, f := range []string{"host", "encryption", "from", "fromName", "user", "secretName"} {
		assert.Equal(t, "string", dig(props, f, "type"), f)
	}
	assert.Equal(t, "integer", dig(props, "port", "type"))
	assert.Equal(t, float64(1), dig(props, "port", "minimum"))
	assert.Equal(t, float64(65535), dig(props, "port", "maximum"))
	assert.Equal(t, []any{"starttls", "tls", "none"}, dig(props, "encryption", "enum"))
	assert.Equal(t, float64(60), dig(props, "fromName", "maxLength"))
	assert.Equal(t, "array", dig(props, "alertsTo", "type"))
	assert.Equal(t, float64(10), dig(props, "alertsTo", "maxItems"))
	assert.Equal(t, "string", dig(props, "alertsTo", "items", "type"))
	for _, f := range []string{"port", "encryption", "fromName"} {
		assert.Nil(t, dig(props, f, "default"), "%s: no default in the schema", f)
	}

	assert.Equal(t, []any{map[string]any{
		"rule":    "!has(self.host) || size(self.host) == 0 || (has(self.from) && size(self.from) > 0)",
		"message": "from is required when host is set",
	}, map[string]any{
		"rule":    "!has(self.user) || size(self.user) == 0 || (has(self.secretName) && size(self.secretName) > 0)",
		"message": "secretName is required when user is set",
	}}, dig(mail, "x-kubernetes-validations"), "a server needs a sender, and a user the Secret of its password")
}

// R.C1: the realm Job runs again when the Secret named appears (Mail, The
// settings): a Secret made after the resource named it, as the condition Mail
// tells an admin to make it, is the run's to write, and so is a change of it,
// a new password in place. The run's fingerprint carries the Secret's
// version, never its data; a Secret that stays as it was starts no run.
func TestRC1TheRealmRunsAgainWhenTheSecretOfThePasswordAppears(t *testing.T) {
	ctx := context.Background()
	e := newRealmEnv(t, mailCR("mailer", "mail-a"))
	e.keycloak(1)
	require.True(t, e.pass(), "a run starts")
	first := e.runs()[0]
	assert.Empty(t, first.Spec.Template.Annotations["zaentrum.io/mail-secret"], "no Secret yet")
	e.end(true, "the realm's mail settings stay as they were until Secret mail-a holds the password")
	e.render()
	assert.False(t, e.pass(), "nothing changed: no run")

	sec := mailSecret("mail-a", withPassword)
	require.NoError(t, e.c.Create(ctx, sec))
	require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "mail-a"}, sec))
	e.render()
	require.True(t, e.pass(), "the Secret appearing runs the realm Job again")
	second := e.runs()[0]
	assert.NotEqual(t, first.Name, second.Name)
	assert.NotEqual(t, first.Annotations[annotationRealmFingerprint], second.Annotations[annotationRealmFingerprint])
	assert.Equal(t, sec.ResourceVersion, second.Spec.Template.Annotations["zaentrum.io/mail-secret"], "the Secret's version, and nothing of its data")
	e.end(true, "the realm sends mail through smtp.example.org:587 now")
	e.render()
	assert.False(t, e.pass(), "the same Secret: no run")

	sec.Data[mailPasswordKey] = []byte("another-password")
	require.NoError(t, e.c.Update(ctx, sec))
	e.render()
	require.True(t, e.pass(), "a change of the Secret runs the realm Job again")
	third := e.runs()[0]
	assert.NotEqual(t, second.Annotations[annotationRealmFingerprint], third.Annotations[annotationRealmFingerprint])
	for _, run := range []batchv1.Job{first, second, third} {
		raw, err := json.Marshal(run.Spec.Template)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "not-a-real-password")
		assert.NotContains(t, string(raw), "another-password")
	}
}

// R.C1: portal-api starts again when the Secret named appears or changes
// (Mail, The settings): a pass reads the Secret's version and the chart puts
// it on portal-api's pod, which reads the password at its start. While no
// Secret is named, or the one named is not there, the pod carries none; a
// Secret that cannot be read defers the render, so that nothing starts again
// on a guess.
func TestRC1PortalAPIStartsAgainWhenTheSecretOfThePasswordAppears(t *testing.T) {
	ctx := context.Background()
	failRead := false
	e := newPlatformEnv(t, mailCR("mailer", "mail-a"), nil)
	e.c = interceptor.NewClient(e.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if failRead && key.Name == "mail-a" {
			return apierrors.NewServiceUnavailable("the API server is busy")
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	e.r.Client = e.c
	annotation := func() string {
		var dep appsv1.Deployment
		require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "portal-api"}, &dep))
		return dep.Spec.Template.Annotations["zaentrum.io/mail-secret"]
	}

	e.reconcile()
	assert.Empty(t, annotation(), "no Secret yet")

	sec := mailSecret("mail-a", withPassword)
	require.NoError(t, e.c.Create(ctx, sec))
	require.NoError(t, e.c.Get(ctx, types.NamespacedName{Namespace: verifyNS, Name: "mail-a"}, sec))
	e.reconcile()
	assert.Equal(t, sec.ResourceVersion, annotation(), "the Secret appearing starts portal-api again")

	sec.Data[mailPasswordKey] = []byte("another-password")
	require.NoError(t, e.c.Update(ctx, sec))
	e.reconcile()
	assert.Equal(t, sec.ResourceVersion, annotation(), "and so does a change of it")
	changed := annotation()

	failRead = true
	res, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: e.key})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "a Secret that cannot be read defers the render")
	assert.Equal(t, changed, annotation(), "and portal-api stays as it is")
	failRead = false

	require.NoError(t, e.c.Delete(ctx, sec))
	e.reconcile()
	assert.Empty(t, annotation(), "the Secret gone: none")

	e.change(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.User, z.Spec.Mail.SecretName = "", "" })
	require.NoError(t, e.c.Create(ctx, mailSecret("mail-a", withPassword)))
	e.reconcile()
	assert.Empty(t, annotation(), "no Secret named: none, whatever is in the namespace")
}

// R.C1: the resource refuses a control character in every mail field, the
// items of alertsTo among them, and a secretName no Secret can have, with the
// words of the chapter (Words of the resource and the realm job, The resource
// refused): “{field} must not hold a control character” and “secretName must
// be a name a Secret can have: lowercase letters, digits, hyphens and dots”.
// A Secret's name is a DNS subdomain of at most 253 characters. The rules'
// regular expressions are RE2, as Go's: they are tried here as CEL runs them.
func TestRC1TheMailSchemaRefusesControlCharactersAndNamesNoSecretCanHave(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	props, _ := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties", "mail", "properties").(map[string]any)
	require.NotNil(t, props, "spec.mail")

	control := "!self.matches('[[:cntrl:]]')"
	for _, f := range []string{"host", "from", "fromName", "user", "secretName"} {
		assert.Contains(t, dig(props, f, "x-kubernetes-validations"), map[string]any{
			"rule": control, "message": f + " must not hold a control character",
		}, f)
	}
	assert.Equal(t, []any{map[string]any{
		"rule": "self.all(a, !a.matches('[[:cntrl:]]'))", "message": "alertsTo must not hold a control character",
	}}, dig(props, "alertsTo", "x-kubernetes-validations"), "every item of alertsTo")
	assert.Nil(t, dig(props, "encryption", "x-kubernetes-validations"), "encryption is one of three words")

	rules, _ := dig(props, "secretName", "x-kubernetes-validations").([]any)
	var name string
	for _, r := range rules {
		if dig(r, "message") == "secretName must be a name a Secret can have: lowercase letters, digits, hyphens and dots" {
			name, _ = dig(r, "rule").(string)
		}
	}
	require.NotEmpty(t, name, "the rule of a Secret's name")
	assert.True(t, strings.HasPrefix(name, "self == '' || (size(self) <= 253 && self.matches('"), name)
	pattern := strings.TrimSuffix(strings.TrimPrefix(name, "self == '' || (size(self) <= 253 && self.matches('"), "'))")
	re := regexp.MustCompile(pattern)
	for _, ok := range []string{"mail-x7k2p", "mail.v2", "a", "0", "zaentrum-mail-2026.11", strings.Repeat("a", 253)} {
		assert.True(t, re.MatchString(ok), "a name a Secret can have: %q", ok)
	}
	for _, bad := range []string{"Mail", "mail_1", "-mail", "mail-", "mail..x", ".mail", "mail.", "mail x", "mäil", "mail/1"} {
		assert.False(t, re.MatchString(bad), "no Secret has the name %q", bad)
	}

	cntrl := regexp.MustCompile("[[:cntrl:]]")
	for c := 0; c < 0x20; c++ {
		assert.True(t, cntrl.MatchString("a"+string(rune(c))+"b"), "control character %#x", c)
	}
	assert.True(t, cntrl.MatchString("a\x7fb"), "DEL")
	for _, ok := range []string{"smtp.example.org", "Media at home", "mäiler", "noreply@example.org", ""} {
		assert.False(t, cntrl.MatchString(ok), "no control character in %q", ok)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// R.C1: the realm Job writes the mail settings, so a change of them — the
// server, the port, the sender, the user, the Secret a new password is in,
// mail set up or no longer — starts a realm run; a change the realm does not
// hold does not.
func TestRC1TheRealmRunsAgainWhenTheMailSettingsChange(t *testing.T) {
	render := func(mutate func(*zaentrumv1alpha1.Zaentrum)) string {
		z := mailCR("mailer", "mail-a")
		mutate(z)
		objs, err := templates.Render(templates.NewValues(z))
		require.NoError(t, err)
		_, hooks := templates.SplitHooks(objs)
		return realmFingerprint(templates.RealmJob(hooks))
	}
	same := render(func(*zaentrumv1alpha1.Zaentrum) {})
	assert.Equal(t, same, render(func(*zaentrumv1alpha1.Zaentrum) {}), "stable")
	for name, mutate := range map[string]func(*zaentrumv1alpha1.Zaentrum){
		"the server":     func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.Host = "mail.example.org" },
		"the port":       func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.Port = 2525 },
		"the encryption": func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.Encryption = "tls" },
		"the sender":     func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.From = "media@example.org" },
		"its name":       func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.FromName = "Media at home" },
		"the user":       func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.User = "media" },
		"a new Secret":   func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.SecretName = "mail-b" },
		"no sign-in":     func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.User, z.Spec.Mail.SecretName = "", "" },
		"no mail":        func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail = zaentrumv1alpha1.MailSpec{} },
	} {
		assert.NotEqual(t, same, render(mutate), name)
	}
	assert.Equal(t, same, render(func(z *zaentrumv1alpha1.Zaentrum) { z.Spec.Mail.AlertsTo = []string{"ops@example.org"} }),
		"the further addresses are portal-api's, not the realm's")
}
