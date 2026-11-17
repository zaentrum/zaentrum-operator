package templates

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The platform's mail settings (spec/portal/operations.md, Mail, The
// settings): spec.mail and the chart's mail values, which portal-api is given
// as environment variables and the bundled realm's Job writes into the realm.

// mailCR is a platform at media.example.org with the mail settings m.
func mailCR(m zaentrumv1alpha1.MailSpec) *zaentrumv1alpha1.Zaentrum {
	z := base("zaentrum")
	z.Spec.Hostname = "media.example.org"
	z.Spec.Mail = m
	return z
}

// portalMailEnv is portal-api's environment that is the platform's mail:
// PORTAL_MAIL_* and PORTAL_PUBLIC_URL.
func portalMailEnv(t *testing.T, objs []*unstructured.Unstructured) map[string]corev1.EnvVar {
	t.Helper()
	out := map[string]corev1.EnvVar{}
	for name, e := range envByName(container(t, objs, "portal-api")) {
		if strings.HasPrefix(name, "PORTAL_MAIL_") || name == "PORTAL_PUBLIC_URL" {
			out[name] = e
		}
	}
	return out
}

// realmMailEnv is the realm Job's environment that is the platform's mail:
// MAIL_*.
func realmMailEnv(t *testing.T, z *zaentrumv1alpha1.Zaentrum) map[string]corev1.EnvVar {
	t.Helper()
	out := map[string]corev1.EnvVar{}
	for name, e := range envByName(realmJob(t, z).Spec.Template.Spec.Containers[0]) {
		if strings.HasPrefix(name, "MAIL_") {
			out[name] = e
		}
	}
	return out
}

func names(env map[string]corev1.EnvVar) []string {
	out := make([]string, 0, len(env))
	for name := range env {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// R.C1: portal-api is given the mail settings as environment variables, with
// exactly the names of the section The settings, only when mail is set up — a
// server and a sender — however the platform is installed: from the resource
// through the operator, or from the chart's values alone. The port follows the
// encryption unless one is named. The user is the resource's own; the
// password is the key password of the Secret the settings name, read
// optionally, so that a Secret that is not there never keeps portal-api from
// starting; and PORTAL_MAIL_SECRET is that Secret's name, so portal-api can
// tell a new password without reading any Secret.
func TestRC1PortalAPIIsGivenTheMailSettingsWhenMailIsSetUp(t *testing.T) {
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":               renderCR(t, base("zaentrum")),
		"demo":                   renderCR(t, demoCR("zaentrum-demo")),
		"helm install":           helmRender(t, nil),
		"a server and no sender": renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{Host: "smtp.example.org"})),
		"a sender and no server": renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{From: "noreply@example.org"})),
		"helm install, a server and no sender": helmRender(t, map[string]interface{}{
			"mail": map[string]interface{}{"host": "smtp.example.org"},
		}),
	} {
		assert.Empty(t, names(portalMailEnv(t, objs)), "%s: mail is not set up, and portal-api is given none of it", name)
	}

	// Set up, with no sign-in: the port is the encryption's own.
	for encryption, port := range map[string]string{"": "587", "starttls": "587", "tls": "465", "none": "25"} {
		objs := renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{
			Host: "smtp.example.org", From: "noreply@example.org", Encryption: encryption,
		}))
		env := portalMailEnv(t, objs)
		want := encryption
		if want == "" {
			want = "starttls"
		}
		assert.Equal(t, []string{
			"PORTAL_MAIL_ALERTS_TO", "PORTAL_MAIL_ENCRYPTION", "PORTAL_MAIL_FROM", "PORTAL_MAIL_FROM_NAME",
			"PORTAL_MAIL_HOST", "PORTAL_MAIL_PORT", "PORTAL_PUBLIC_URL",
		}, names(env), "%q: without a Secret there is no user and no password", encryption)
		assert.Equal(t, "smtp.example.org", env["PORTAL_MAIL_HOST"].Value)
		assert.Equal(t, port, env["PORTAL_MAIL_PORT"].Value, "the port of %q", want)
		assert.Equal(t, want, env["PORTAL_MAIL_ENCRYPTION"].Value)
		assert.Equal(t, "noreply@example.org", env["PORTAL_MAIL_FROM"].Value)
		assert.Equal(t, "zaentrum", env["PORTAL_MAIL_FROM_NAME"].Value, "the sender's name by default")
		assert.Equal(t, "", env["PORTAL_MAIL_ALERTS_TO"].Value, "no further addresses")
		assert.Equal(t, "http://media.example.org", env["PORTAL_PUBLIC_URL"].Value, "the links in a mail start with the public address")
	}

	// Everything named: the port as named, the sender's name, the further
	// addresses separated by commas, the user, and the Secret with its
	// password.
	all := zaentrumv1alpha1.MailSpec{
		Host: "smtp.example.org", Port: 2525, Encryption: "tls", From: "noreply@example.org", FromName: "Media at home",
		User: "mailer", SecretName: "mail-x7k2p", AlertsTo: []string{"ops@example.org", "admin@example.org"},
	}
	values := map[string]interface{}{"mail": map[string]interface{}{
		"host": "smtp.example.org", "port": 2525, "encryption": "tls", "from": "noreply@example.org",
		"fromName": "Media at home", "user": "mailer", "secretName": "mail-x7k2p",
		"alertsTo": []interface{}{"ops@example.org", "admin@example.org"},
	}, "global": map[string]interface{}{"hostname": "media.example.org"}}
	external := mailCR(all)
	external.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	external.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	for name, objs := range map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, mailCR(all)),
		"helm install": helmRender(t, values),
		// Alert mail goes to the further addresses alone there, and portal-api
		// sends it all the same.
		"external identity": renderCR(t, external),
	} {
		env := portalMailEnv(t, objs)
		assert.Equal(t, []string{
			"PORTAL_MAIL_ALERTS_TO", "PORTAL_MAIL_ENCRYPTION", "PORTAL_MAIL_FROM", "PORTAL_MAIL_FROM_NAME", "PORTAL_MAIL_HOST",
			"PORTAL_MAIL_PASSWORD", "PORTAL_MAIL_PORT", "PORTAL_MAIL_SECRET", "PORTAL_MAIL_USERNAME", "PORTAL_PUBLIC_URL",
		}, names(env), name)
		assert.Equal(t, "2525", env["PORTAL_MAIL_PORT"].Value, "%s: the port named", name)
		assert.Equal(t, "tls", env["PORTAL_MAIL_ENCRYPTION"].Value, name)
		assert.Equal(t, "Media at home", env["PORTAL_MAIL_FROM_NAME"].Value, name)
		assert.Equal(t, "ops@example.org,admin@example.org", env["PORTAL_MAIL_ALERTS_TO"].Value, name)
		assert.Equal(t, "mailer", env["PORTAL_MAIL_USERNAME"].Value, "%s: the user is the resource's", name)
		assert.Nil(t, env["PORTAL_MAIL_USERNAME"].ValueFrom, "%s: no Secret holds the user", name)
		secretRef(t, env, "PORTAL_MAIL_PASSWORD", "mail-x7k2p", "password")
		assert.Equal(t, "mail-x7k2p", env["PORTAL_MAIL_SECRET"].Value, "%s: the Secret's name, which a new password changes", name)
	}

	// A user without a Secret is given its name alone; a Secret without a
	// user its password and its name.
	userOnly := portalMailEnv(t, renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{
		Host: "smtp.example.org", From: "noreply@example.org", User: "mailer",
	})))
	assert.Contains(t, userOnly, "PORTAL_MAIL_USERNAME")
	assert.NotContains(t, userOnly, "PORTAL_MAIL_PASSWORD")
	assert.NotContains(t, userOnly, "PORTAL_MAIL_SECRET")
	secretOnly := portalMailEnv(t, renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{
		Host: "smtp.example.org", From: "noreply@example.org", SecretName: "mail-x7k2p",
	})))
	assert.NotContains(t, secretOnly, "PORTAL_MAIL_USERNAME")
	secretRef(t, secretOnly, "PORTAL_MAIL_PASSWORD", "mail-x7k2p", "password")
	assert.Equal(t, "mail-x7k2p", secretOnly["PORTAL_MAIL_SECRET"].Value)

	// The public address is the one users reach the platform at: https behind
	// a proxy that terminates TLS, with a certificate of its own, and with
	// OpenShift's Routes.
	proxy := mailCR(all)
	proxy.Spec.Identity.IssuerScheme = "https"
	tls := mailCR(all)
	tls.Spec.TLS = &zaentrumv1alpha1.TLSSpec{}
	demo := demoCR("zaentrum-demo")
	demo.Spec.Hostname = "media.example.org"
	demo.Spec.Mail = all
	for name, z := range map[string]*zaentrumv1alpha1.Zaentrum{"https proxy": proxy, "spec.tls": tls, "routes": demo} {
		assert.Equal(t, "https://media.example.org", portalMailEnv(t, renderCR(t, z))["PORTAL_PUBLIC_URL"].Value, name)
	}
}

// R.C1: with the bundled identity provider the realm Job is given the same
// settings — the server, the port, the encryption, the sender, the user, and
// the password of the Secret — and writes them into the realm's mail
// settings; without mail it is given none of them and clears the realm's;
// while the Secret holds no password, or a user has no Secret named, the
// realm's stay as they were. It
// writes them before it moves where the admin console signs in, which ends
// its own token. With an external provider there is no realm Job at all.
func TestRC1TheRealmJobWritesTheMailSettingsIntoTheRealm(t *testing.T) {
	assert.Empty(t, names(realmMailEnv(t, base("zaentrum"))), "no mail: the Job is told none, and clears the realm's")
	assert.Empty(t, names(realmMailEnv(t, mailCR(zaentrumv1alpha1.MailSpec{Host: "smtp.example.org"}))),
		"a server and no sender is no mail")

	env := realmMailEnv(t, mailCR(zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org", Encryption: "none"}))
	assert.Equal(t, []string{"MAIL_ENCRYPTION", "MAIL_FROM", "MAIL_FROM_NAME", "MAIL_HOST", "MAIL_PORT"}, names(env),
		"without a user the realm does not sign in")
	assert.Equal(t, "smtp.example.org", env["MAIL_HOST"].Value)
	assert.Equal(t, "25", env["MAIL_PORT"].Value, "the port of none")
	assert.Equal(t, "none", env["MAIL_ENCRYPTION"].Value)
	assert.Equal(t, "noreply@example.org", env["MAIL_FROM"].Value)
	assert.Equal(t, "zaentrum", env["MAIL_FROM_NAME"].Value)

	env = realmMailEnv(t, mailCR(zaentrumv1alpha1.MailSpec{
		Host: "smtp.example.org", From: "noreply@example.org", FromName: "Media at home", User: "mailer", SecretName: "mail-x7k2p",
	}))
	assert.Equal(t, []string{"MAIL_ENCRYPTION", "MAIL_FROM", "MAIL_FROM_NAME", "MAIL_HOST", "MAIL_PASSWORD", "MAIL_PORT",
		"MAIL_SECRET", "MAIL_USERNAME"}, names(env))
	assert.Equal(t, "587", env["MAIL_PORT"].Value, "the port of starttls, the default")
	assert.Equal(t, "starttls", env["MAIL_ENCRYPTION"].Value)
	assert.Equal(t, "Media at home", env["MAIL_FROM_NAME"].Value)
	assert.Equal(t, "mailer", env["MAIL_USERNAME"].Value, "the user is the resource's")
	assert.Nil(t, env["MAIL_USERNAME"].ValueFrom)
	assert.Equal(t, "mail-x7k2p", env["MAIL_SECRET"].Value)
	secretRef(t, env, "MAIL_PASSWORD", "mail-x7k2p", "password")

	// The chart's values alone give the Job the same.
	_, hooks := SplitHooks(helmRender(t, map[string]interface{}{"mail": map[string]interface{}{
		"host": "smtp.example.org", "from": "noreply@example.org", "encryption": "tls",
	}}))
	require.NotNil(t, RealmJob(hooks))
	containers, _, _ := unstructured.NestedSlice(RealmJob(hooks).Object, "spec", "template", "spec", "containers")
	helmEnv := map[string]string{}
	for _, e := range containers[0].(map[string]interface{})["env"].([]interface{}) {
		em := e.(map[string]interface{})
		if v, ok := em["value"].(string); ok {
			helmEnv[em["name"].(string)] = v
		}
	}
	assert.Equal(t, "465", helmEnv["MAIL_PORT"], "helm install: the port of tls")
	assert.Equal(t, "smtp.example.org", helmEnv["MAIL_HOST"])

	// What the Job does with them: the realm's smtpServer, read — every
	// setting of it: kcadm answers smtpServer alone with an empty map — and
	// written through the admin API as the rest, in kcadm's merge, cleared
	// without mail, before the master realm's frontend URL, which is last.
	// What the run does is held by the tests that run it (mailrun_test.go).
	raw, err := os.ReadFile("../../platform/chart/files/realm-config.sh")
	require.NoError(t, err)
	script := string(raw)
	read := strings.Index(script, `have=$(kc get "realms/$realm" --fields 'smtpServer(*)')`)
	cleared := strings.Index(script, `kc update "realms/$realm" -s 'smtpServer={}'`)
	written := strings.Index(script, `printf '{"smtpServer":{%s}}' "$body" | kc update "realms/$realm" -f - -m`)
	console := strings.Index(script, `kc update realms/master -s "attributes.frontendUrl=$MASTER_FRONTEND_URL"`)
	require.Positive(t, read, "the Job reads the realm's mail settings")
	assert.NotContains(t, script, `--fields smtpServer)`, "asked for smtpServer alone, kcadm answers an empty map")
	require.Positive(t, cleared, "the Job clears them without mail")
	require.Positive(t, written, "the Job writes them with mail")
	require.Positive(t, console)
	assert.Less(t, written, console, "the mail settings are written before the console's sign-in moves")
	assert.Less(t, cleared, console)
	// Keycloak's names of the settings: the server, the port, the sender and
	// its name, the encryption — ssl from the start, or starttls — and the
	// sign-in with its user.
	assert.Contains(t, script, `settings=(host "$MAIL_HOST" port "${MAIL_PORT:-}" from "${MAIL_FROM:-}" fromDisplayName "${MAIL_FROM_NAME:-}"`)
	assert.Contains(t, script, `ssl "$ssl" starttls "$starttls" auth "$auth")`)
	assert.Contains(t, script, `settings+=(user "$MAIL_USERNAME")`)
	assert.Contains(t, script, `body+=",\"password\":$(js "${MAIL_PASSWORD:-}")"`, "the password, on stdin and never as an argument")
	assert.Contains(t, script, `elif [ -n "${MAIL_SECRET:-}" ] && [ -z "${MAIL_PASSWORD+set}" ]; then`,
		"while the Secret holds no password, the realm's mail settings stay as they were")
	assert.Contains(t, script, `elif [ -n "${MAIL_USERNAME:-}" ] && [ -z "${MAIL_SECRET:-}" ]; then`,
		"a user with no Secret named, where the resource's rule did not run, leaves them as they were too")
	// What the run says, in the words of the chapter.
	for _, words := range []string{
		`the realm sends no mail now`,
		`the realm sends mail through $MAIL_HOST:${MAIL_PORT:-}`,
		`the realm's mail settings stay as they were until Secret $MAIL_SECRET holds the password`,
		`the realm's mail settings stay as they were until spec.mail names a Secret for the password of $MAIL_USERNAME`,
		`cannot read the mail settings of realm $realm: $(why)`,
		`cannot clear the mail settings of realm $realm: $(why)`,
		`cannot set the mail settings of realm $realm: $(why)`,
		`mail.encryption is ${MAIL_ENCRYPTION}: use starttls, tls or none`,
	} {
		assert.Contains(t, script, words)
	}

	ext := base("zaentrum-beta")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	ext.Spec.Mail = zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org"}
	_, hooks = SplitHooks(renderCR(t, ext))
	assert.Nil(t, RealmJob(hooks), "an external provider sends its own")
}

// R.C1: the version of the Secret of the mail server's password, which the
// reconciler reads each pass (Values.MailSecretVersion), goes on portal-api's
// pod and on the realm Job's alike, as zaentrum.io/mail-secret: its appearing,
// or a change of it, starts portal-api again and runs the realm Job again.
// Only while mail is set up and a Secret is named; never under plain Helm,
// where the operator reads nothing.
func TestRC1TheSecretsVersionIsOnPortalAPIAndOnTheRealmJob(t *testing.T) {
	annotations := func(z *zaentrumv1alpha1.Zaentrum, version string) (portal, realm map[string]string) {
		v := NewValues(z)
		v.MailSecretVersion = version
		objs, err := Render(v)
		require.NoError(t, err)
		portal = typedDeployment(t, objs, "portal-api").Spec.Template.Annotations
		_, hooks := SplitHooks(objs)
		if job := RealmJob(hooks); job != nil {
			realm, _, _ = unstructured.NestedStringMap(job.Object, "spec", "template", "metadata", "annotations")
		}
		return portal, realm
	}
	signIn := zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org", User: "mailer", SecretName: "mail-x7k2p"}

	portal, realm := annotations(mailCR(signIn), "4711")
	assert.Equal(t, "4711", portal["zaentrum.io/mail-secret"], "portal-api")
	assert.Equal(t, "4711", realm["zaentrum.io/mail-secret"], "the realm Job")
	changed, changedRealm := annotations(mailCR(signIn), "4712")
	assert.NotEqual(t, portal, changed, "a change of the Secret is a new pod template")
	assert.NotEqual(t, realm, changedRealm)

	portal, realm = annotations(mailCR(signIn), "")
	assert.NotContains(t, portal, "zaentrum.io/mail-secret", "the Secret is not there")
	assert.NotContains(t, realm, "zaentrum.io/mail-secret")
	noSignIn := zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org"}
	for name, z := range map[string]*zaentrumv1alpha1.Zaentrum{"no Secret named": mailCR(noSignIn), "no mail": base("zaentrum")} {
		portal, realm = annotations(z, "4711")
		assert.NotContains(t, portal, "zaentrum.io/mail-secret", name)
		assert.NotContains(t, realm, "zaentrum.io/mail-secret", name)
	}

	objs := helmRender(t, map[string]interface{}{"mail": map[string]interface{}{
		"host": "smtp.example.org", "from": "noreply@example.org", "user": "mailer", "secretName": "mail-x7k2p",
	}})
	assert.NotContains(t, typedDeployment(t, objs, "portal-api").Spec.Template.Annotations, "zaentrum.io/mail-secret", "helm install")
}
