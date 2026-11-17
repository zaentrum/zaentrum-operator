package templates

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The realm Job's mail step, run (kcadm_test.go): what the realm's mail
// settings are after a run, what the run says, and that every other setting
// of the realm keeps its value through it (spec/portal/operations.md, Mail,
// The settings).

// anImportedRealm is the chart's realm as Keycloak answers it, its other
// settings at values that are not the defaults — set in the identity
// provider's console, say — so that a write that sets one back is seen.
func anImportedRealm() map[string]any {
	return map[string]any{
		"realm": "zaentrum", "smtpServer": map[string]any{}, "resetPasswordAllowed": false,
		"oauth2DeviceCodeLifespan": float64(900), "oauth2DevicePollingInterval": float64(7),
		"clientSessionIdleTimeout": float64(1200),
		"attributes": map[string]any{
			"cibaBackchannelTokenDeliveryMode": "ping", "cibaExpiresIn": "200", "cibaInterval": "9",
			"cibaAuthRequestedUserHint": "id_token_hint", "parRequestUriLifespan": "90", "proof.marker": "kept",
		},
	}
}

// otherSettings are anImportedRealm's settings that are not the mail's.
func otherSettings(realm map[string]any) map[string]any {
	attrs, _ := realm["attributes"].(map[string]any)
	return map[string]any{
		"oauth2DeviceCodeLifespan": realm["oauth2DeviceCodeLifespan"], "oauth2DevicePollingInterval": realm["oauth2DevicePollingInterval"],
		"clientSessionIdleTimeout": realm["clientSessionIdleTimeout"], "resetPasswordAllowed": realm["resetPasswordAllowed"],
		"cibaBackchannelTokenDeliveryMode": attrs["cibaBackchannelTokenDeliveryMode"], "cibaExpiresIn": attrs["cibaExpiresIn"],
		"cibaInterval": attrs["cibaInterval"], "cibaAuthRequestedUserHint": attrs["cibaAuthRequestedUserHint"],
		"parRequestUriLifespan": attrs["parRequestUriLifespan"], "proof.marker": attrs["proof.marker"],
	}
}

func withMail(smtp map[string]any) map[string]any {
	r := anImportedRealm()
	r["smtpServer"] = smtp
	return r
}

// mailOf is the environment of a realm Job whose mail is set up through
// smtp.example.org, with what more is given.
func mailOf(more ...string) map[string]string {
	env := map[string]string{"MAIL_HOST": "smtp.example.org", "MAIL_PORT": "587", "MAIL_ENCRYPTION": "starttls",
		"MAIL_FROM": "noreply@example.org", "MAIL_FROM_NAME": "zaentrum"}
	for i := 0; i+1 < len(more); i += 2 {
		env[more[i]] = more[i+1]
	}
	return env
}

var noSignIn = map[string]any{"host": "smtp.example.org", "port": "587", "from": "noreply@example.org",
	"fromDisplayName": "zaentrum", "ssl": "false", "starttls": "true", "auth": "false"}

func signedIn(password string) map[string]any {
	m := map[string]any{"user": "mailer", "password": password}
	for k, v := range noSignIn {
		m[k] = v
	}
	m["auth"] = "true"
	return m
}

func mailRun(r *realmRun, env map[string]string) stepRun {
	r.t.Helper()
	return r.run([]string{"mail"}, []string{"mail"}, env)
}

// R.C1: the realm Job's mail step, run with bash against a stand-in kcadm that
// answers as Keycloak 26.0.7's does: it writes the platform's mail settings
// into the realm, or clears them, and says so; it writes a password it cannot
// compare every time; it waits while the password's Secret is missing; each
// of its writes replaces the mail settings whole and leaves every other
// setting of the realm at the value it had; and no password is ever an
// argument, an output or a reason.
func TestRC1TheMailStepRuns(t *testing.T) {
	t.Run("R.C1 no mail, and none in the realm: nothing written, nothing said", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		out := mailRun(r, nil)
		require.Zero(t, out.exit, out.stderr)
		assert.Empty(t, out.said["mail"])
		assert.Empty(t, r.writes())
	})

	t.Run("R.C1 mail set up, no sign-in: the settings written, and said", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		out := mailRun(r, mailOf())
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends mail through smtp.example.org:587 now", out.said["mail"])
		assert.Equal(t, noSignIn, r.realm()["smtpServer"])
		assert.Equal(t, []string{"merge smtpServer"}, r.writes(), "a merge, which changed the mail settings alone")
	})

	t.Run("R.C1 the same again: nothing written, said as it is", func(t *testing.T) {
		r := newRealmRun(t, withMail(noSignIn))
		out := mailRun(r, mailOf())
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends mail through smtp.example.org:587", out.said["mail"])
		assert.Empty(t, r.writes())
	})

	t.Run("R.C1 tls and none: ssl and starttls as the encryption says", func(t *testing.T) {
		for enc, want := range map[string][2]string{"tls": {"true", "false"}, "none": {"false", "false"}, "starttls": {"false", "true"}} {
			r := newRealmRun(t, anImportedRealm())
			out := mailRun(r, mailOf("MAIL_ENCRYPTION", enc, "MAIL_PORT", "465"))
			require.Zero(t, out.exit, out.stderr)
			smtp := r.realm()["smtpServer"].(map[string]any)
			assert.Equal(t, want[0], smtp["ssl"], enc)
			assert.Equal(t, want[1], smtp["starttls"], enc)
			assert.Equal(t, "465", smtp["port"], enc)
		}
	})

	t.Run("R.C1 a sign-in: the user, and the password as it was given", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", "s3cr\"et\\pa ss"))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, signedIn("s3cr\"et\\pa ss"), r.realm()["smtpServer"])
	})

	t.Run("R.C1 a sign-in again: the password is written again, as it cannot be compared", func(t *testing.T) {
		r := newRealmRun(t, withMail(signedIn("s3cret")))
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", "s3cret"))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends mail through smtp.example.org:587", out.said["mail"], "said as it is")
		assert.Len(t, r.writes(), 1, "written, the password with the settings")
		assert.Equal(t, signedIn("s3cret"), r.realm()["smtpServer"])
	})

	t.Run("R.C1 a new password, in a new Secret: written", func(t *testing.T) {
		r := newRealmRun(t, withMail(signedIn("s3cret")))
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-2", "MAIL_PASSWORD", "another"))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, signedIn("another"), r.realm()["smtpServer"])
	})

	t.Run("R.C1 the Secret named holds no password: nothing written, the settings stay", func(t *testing.T) {
		r := newRealmRun(t, withMail(signedIn("s3cret")))
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-2"))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm's mail settings stay as they were until Secret mail-2 holds the password", out.said["mail"])
		assert.Empty(t, r.writes())
		assert.Equal(t, signedIn("s3cret"), r.realm()["smtpServer"])
	})

	t.Run("R.C1 a user and no Secret named: nothing written, the settings stay", func(t *testing.T) {
		r := newRealmRun(t, withMail(noSignIn))
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer"))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm's mail settings stay as they were until spec.mail names a Secret for the password of mailer", out.said["mail"])
		assert.Empty(t, r.writes())
	})

	t.Run("R.C1 the Secret appears after the resource named it: the next run writes the password", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		waiting := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1"))
		require.Zero(t, waiting.exit, waiting.stderr)
		assert.Empty(t, r.writes(), "no Secret yet: nothing written")
		appeared := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", "s3cret"))
		require.Zero(t, appeared.exit, appeared.stderr)
		assert.Equal(t, "the realm sends mail through smtp.example.org:587 now", appeared.said["mail"])
		assert.Equal(t, signedIn("s3cret"), r.realm()["smtpServer"], "the password the Secret holds")
	})

	t.Run("R.C1 no sign-in again: the user and the password go", func(t *testing.T) {
		r := newRealmRun(t, withMail(signedIn("s3cret")))
		out := mailRun(r, mailOf())
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, noSignIn, r.realm()["smtpServer"])
	})

	t.Run("R.C1 a mail setting someone added by hand goes: the settings are replaced whole", func(t *testing.T) {
		smtp := map[string]any{"replyTo": "reply@example.org", "envelopeFrom": "bounce@example.org"}
		for k, v := range noSignIn {
			smtp[k] = v
		}
		r := newRealmRun(t, withMail(smtp))
		out := mailRun(r, mailOf())
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, noSignIn, r.realm()["smtpServer"])
		assert.Equal(t, "the realm sends mail through smtp.example.org:587 now", out.said["mail"])
	})

	t.Run("R.C1 mail no longer set up: the settings cleared, once", func(t *testing.T) {
		r := newRealmRun(t, withMail(signedIn("s3cret")))
		out := mailRun(r, nil)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends no mail now", out.said["mail"])
		assert.Equal(t, map[string]any{}, r.realm()["smtpServer"])
		again := mailRun(r, nil)
		require.Zero(t, again.exit, again.stderr)
		assert.Empty(t, again.said["mail"], "nothing said")
		assert.Len(t, r.writes(), 1, "nothing written again")
	})

	t.Run("R.C1 every other setting keeps its value through every write", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		want := otherSettings(anImportedRealm())
		for _, env := range []map[string]string{
			mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", "s3cret"),
			mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", "s3cret"),
			mailOf(),
			nil,
		} {
			out := mailRun(r, env)
			require.Zero(t, out.exit, out.stderr)
			assert.Equal(t, want, otherSettings(r.realm()), "after %q", out.said["mail"])
		}
		require.Len(t, r.writes(), 4, "a sign-in, again, none, cleared")
		for _, w := range r.writes() {
			kind, changed, _ := strings.Cut(w, " ")
			assert.Equal(t, "merge", kind, w)
			for _, f := range strings.Split(strings.TrimSpace(changed), ",") {
				assert.Contains(t, []string{"", "smtpServer"}, f, "a write that changed the mail settings alone: %s", w)
			}
		}
	})

	t.Run("R.C1 an encryption none of the three: the run fails, and says why", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		out := mailRun(r, mailOf("MAIL_ENCRYPTION", "ssl"))
		assert.Equal(t, 1, out.exit)
		assert.Equal(t, "mail.encryption is ssl: use starttls, tls or none", out.termination)
		assert.Empty(t, r.writes())
	})

	t.Run("R.C1 a password with control characters: written, and in no output", func(t *testing.T) {
		password := "pa\x01ss\x1bw\x7f\"o\\r\td\x02"
		r := newRealmRun(t, anImportedRealm())
		out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", password))
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, signedIn(password), r.realm()["smtpServer"], "a body kcadm takes as JSON, the password as it was")
		for name, s := range map[string]string{"stdout": out.stdout, "stderr": out.stderr, "termination": out.termination,
			"calls": strings.Join(r.lines("calls.log"), "\n"), "writes": strings.Join(r.writes(), "\n")} {
			assert.NotContains(t, s, password, name)
			assert.NotContains(t, s, "pa\x01ss", name)
		}
	})

	t.Run("R.C1 kcadm's complaint quotes the body: the reason holds no password", func(t *testing.T) {
		for _, password := range []string{"s3cret-pw", "pa\x01ss\"w\\o\nrd"} {
			r := newRealmRun(t, anImportedRealm())
			env := mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", password,
				kcadmFail, `"password"`, kcadmWhy, "Failed to update realm: {body}")
			out := mailRun(r, env)
			assert.Equal(t, 1, out.exit)
			assert.True(t, strings.HasPrefix(out.termination, "cannot set the mail settings of realm zaentrum: Failed to update realm: {"), out.termination)
			assert.Contains(t, out.termination, `"password":"**********"`, "the password taken out")
			esc := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\x01", "\\u0001").Replace(password)
			for name, s := range map[string]string{"termination": out.termination, "stdout": out.stdout, "stderr": out.stderr} {
				assert.NotContains(t, s, password, name)
				assert.NotContains(t, s, esc, name)
			}
			assert.Empty(t, r.writes())
		}
	})

	t.Run("R.C1 no password is ever an argument", func(t *testing.T) {
		r := newRealmRun(t, anImportedRealm())
		for _, pw := range []string{"s3cret", "another"} {
			out := mailRun(r, mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-1", "MAIL_PASSWORD", pw))
			require.Zero(t, out.exit, out.stderr)
		}
		calls := strings.Join(r.lines("calls.log"), "\n")
		assert.NotContains(t, calls, "s3cret")
		assert.NotContains(t, calls, "another")
		assert.Contains(t, calls, "update realms/zaentrum -f - -m", "the body on stdin")
	})
}
