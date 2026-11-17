package templates

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The realm Job's password reset step, run (kcadm_test.go), alone over a realm
// in each state and after the mail step: the reset follows the realm's mail
// settings, a realm without an email theme gets the chart's, and every other
// setting of the realm keeps its value (spec/portal/operations.md, Mail, The
// settings, and The sign-in page).

const (
	resetOn  = "the sign-in page offers a password reset now"
	resetOff = "the sign-in page offers no password reset now: the realm sends no mail"
	themeSet = "the realm's email theme is zaentrum now"
)

// realmAs is anImportedRealm with the reset, the email theme (nil: none) and
// the mail settings given.
func realmAs(reset bool, theme any, smtp map[string]any) map[string]any {
	r := anImportedRealm()
	r["resetPasswordAllowed"] = reset
	if theme != nil {
		r["emailTheme"] = theme
	}
	r["smtpServer"] = smtp
	return r
}

var theTheme = map[string]string{"EMAIL_THEME": "zaentrum"}

func resetRun(r *realmRun, env map[string]string) stepRun {
	r.t.Helper()
	return r.run([]string{"reset"}, []string{"theme", "reset"}, env)
}

func mailAndResetRun(r *realmRun, env map[string]string) stepRun {
	r.t.Helper()
	all := map[string]string{"EMAIL_THEME": "zaentrum"}
	for k, v := range env {
		all[k] = v
	}
	return r.run([]string{"mail", "reset"}, []string{"mail", "theme", "reset"}, all)
}

// R.C5: the realm Job's password reset step, run with bash against a stand-in
// kcadm that answers as Keycloak 26.0.7's does: the reset is on while the
// realm's mail settings name a server and off while they name none; a realm
// without an email theme gets the chart's, and one someone chose stays; each
// write is a merge, so every other setting keeps its value; a failure says
// why, in the words of the chapter.
func TestRC5TheResetStepRuns(t *testing.T) {
	t.Run("R.C5 a realm as the import makes it, no mail: the chart's theme, the reset stays off", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, nil, map[string]any{}))
		out := resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, themeSet, out.said["theme"])
		assert.Empty(t, out.said["reset"], "nothing to switch, nothing said")
		assert.Equal(t, "zaentrum", r.realm()["emailTheme"])
		assert.Equal(t, false, r.realm()["resetPasswordAllowed"])
		assert.Equal(t, []string{"merge emailTheme"}, r.writes())
	})

	t.Run("R.C5 so already: nothing written, nothing said", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, "zaentrum", map[string]any{}))
		out := resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Empty(t, out.said["theme"])
		assert.Empty(t, out.said["reset"])
		assert.Empty(t, r.writes())
	})

	t.Run("R.C5 a server in the realm's mail settings: the reset switched on", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, "zaentrum", noSignIn))
		out := resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, resetOn, out.said["reset"])
		assert.Equal(t, true, r.realm()["resetPasswordAllowed"])
		assert.Equal(t, []string{"merge resetPasswordAllowed"}, r.writes())
		again := resetRun(r, theTheme)
		require.Zero(t, again.exit, again.stderr)
		assert.Empty(t, again.said["reset"], "so already")
		assert.Len(t, r.writes(), 1)
	})

	t.Run("R.C5 no server: the reset switched off", func(t *testing.T) {
		for name, smtp := range map[string]map[string]any{"none": {}, "an empty host": {"host": "", "port": "587"}} {
			r := newRealmRun(t, realmAs(true, "zaentrum", smtp))
			out := resetRun(r, theTheme)
			require.Zero(t, out.exit, out.stderr)
			assert.Equal(t, resetOff, out.said["reset"], name)
			assert.Equal(t, false, r.realm()["resetPasswordAllowed"], name)
		}
	})

	t.Run("R.C5 an email theme someone chose stays; an empty one is none", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, "keycloak", map[string]any{}))
		out := resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "keycloak", r.realm()["emailTheme"])
		assert.Empty(t, r.writes())
		r = newRealmRun(t, realmAs(false, "", map[string]any{}))
		out = resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, themeSet, out.said["theme"])
		assert.Equal(t, "zaentrum", r.realm()["emailTheme"])
	})

	t.Run("R.C5 without EMAIL_THEME the theme is left alone, and the reset switched all the same", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, nil, noSignIn))
		out := resetRun(r, nil)
		require.Zero(t, out.exit, out.stderr)
		assert.NotContains(t, r.realm(), "emailTheme")
		assert.Equal(t, resetOn, out.said["reset"])
	})

	t.Run("R.C5 the mail server's password stays through the switch", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, "zaentrum", signedIn("kept-pw")))
		out := resetRun(r, theTheme)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, signedIn("kept-pw"), r.realm()["smtpServer"], "a merge puts the masked password back, and Keycloak keeps its own")
		assert.NotContains(t, strings.Join(r.lines("calls.log"), "\n"), "kept-pw")
	})

	t.Run("R.C5 every other setting keeps its value through the theme's write and the reset on and off", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, nil, map[string]any{}))
		want := otherSettings(anImportedRealm())
		delete(want, "resetPasswordAllowed")
		for _, smtp := range []map[string]any{{}, noSignIn, {}} {
			realm := r.realm()
			realm["smtpServer"] = smtp
			saveRealm(r.dir, realm)
			out := resetRun(r, theTheme)
			require.Zero(t, out.exit, out.stderr)
			got := otherSettings(r.realm())
			delete(got, "resetPasswordAllowed")
			assert.Equal(t, want, got, "after %q %q", out.said["theme"], out.said["reset"])
		}
		assert.Equal(t, []string{"merge emailTheme", "merge resetPasswordAllowed", "merge resetPasswordAllowed"}, r.writes(),
			"the theme, the reset on, the reset off: each a merge that changed what it names alone")
	})

	t.Run("R.C5 a failure says why, and writes nothing after it", func(t *testing.T) {
		for name, c := range map[string]struct{ fail, why, want, writes string }{
			"the realm cannot be read": {"smtpServer(*),resetPasswordAllowed,emailTheme", "HTTP error 503",
				"cannot read realm zaentrum: HTTP error 503", ""},
			"the theme cannot be set": {"emailTheme=", "HTTP error 403: forbidden",
				"cannot set the email theme of realm zaentrum: HTTP error 403: forbidden", ""},
			"the reset cannot be switched": {"resetPasswordAllowed=", "HTTP error 500",
				"cannot switch the password reset of realm zaentrum: HTTP error 500", "merge emailTheme"},
		} {
			r := newRealmRun(t, realmAs(false, nil, noSignIn))
			out := resetRun(r, map[string]string{"EMAIL_THEME": "zaentrum", kcadmFail: c.fail, kcadmWhy: c.why})
			assert.Equal(t, 1, out.exit, name)
			assert.Equal(t, c.want, out.termination, name)
			assert.Equal(t, c.writes, strings.Join(r.writes(), ","), name)
		}
	})

	t.Run("R.C5 a new realm, no mail: the reset stays off", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, nil, map[string]any{}))
		out := mailAndResetRun(r, nil)
		require.Zero(t, out.exit, out.stderr)
		assert.Empty(t, out.said["mail"])
		assert.Empty(t, out.said["reset"], "nothing said of the reset")
		assert.Equal(t, themeSet, out.said["theme"], "the theme is given on the first run")
		assert.Equal(t, []string{"merge emailTheme"}, r.writes(), "nothing written of the reset")
		again := mailAndResetRun(r, nil)
		require.Zero(t, again.exit, again.stderr)
		assert.Equal(t, map[string]string{"mail": "", "theme": "", "reset": ""}, again.said, "the next run: nothing said")
		assert.Len(t, r.writes(), 1, "and nothing written")
	})

	t.Run("R.C5 a new realm, mail: the settings written, then the reset on", func(t *testing.T) {
		r := newRealmRun(t, realmAs(false, nil, map[string]any{}))
		out := mailAndResetRun(r, mailOf())
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends mail through smtp.example.org:587 now", out.said["mail"])
		assert.Equal(t, resetOn, out.said["reset"])
		assert.Equal(t, []string{"merge smtpServer", "merge emailTheme", "merge resetPasswordAllowed"}, r.writes())
	})

	t.Run("R.C5 mail removed: cleared, then off", func(t *testing.T) {
		r := newRealmRun(t, realmAs(true, "zaentrum", noSignIn))
		out := mailAndResetRun(r, nil)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm sends no mail now", out.said["mail"])
		assert.Equal(t, resetOff, out.said["reset"])
		assert.Equal(t, false, r.realm()["resetPasswordAllowed"])
	})

	t.Run("R.C5 the Secret named holds no password: the reset stays with the realm's settings", func(t *testing.T) {
		waiting := mailOf("MAIL_USERNAME", "mailer", "MAIL_SECRET", "mail-2")
		r := newRealmRun(t, realmAs(true, "zaentrum", signedIn("kept-pw")))
		out := mailAndResetRun(r, waiting)
		require.Zero(t, out.exit, out.stderr)
		assert.Equal(t, "the realm's mail settings stay as they were until Secret mail-2 holds the password", out.said["mail"])
		assert.Empty(t, out.said["reset"], "a server is still in the realm: on, as it was")
		assert.Equal(t, true, r.realm()["resetPasswordAllowed"])
		r = newRealmRun(t, realmAs(false, "zaentrum", map[string]any{}))
		out = mailAndResetRun(r, waiting)
		require.Zero(t, out.exit, out.stderr)
		assert.Empty(t, out.said["reset"], "none is: off, as it was")
		assert.Equal(t, false, r.realm()["resetPasswordAllowed"])
		assert.Empty(t, r.writes())
	})
}
