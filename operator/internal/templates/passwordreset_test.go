package templates

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// “Forgot password?” only where mail is ready (spec/portal/operations.md,
// Mail, The sign-in page): the realm Job switches the bundled realm's password
// reset with the realm's mail, the chart's sign-in page says whom to ask
// without it, and the chart's theme gives the reset mail the platform's words.

// themeFiles is the chart's Keycloak theme as the render's keycloak-theme
// ConfigMap carries it: its files by name.
func themeFiles(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	cm := find(t, objs, "ConfigMap", "keycloak-theme")
	require.NotNil(t, cm, "the render has no theme")
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	require.NoError(t, err)
	return data
}

// themeLayout is the script the keycloak Deployment's init container
// theme-layout lays the theme's files out with, from the ConfigMap at /src.
func themeLayout(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	for _, c := range typedDeployment(t, objs, "keycloak").Spec.Template.Spec.InitContainers {
		if c.Name == "theme-layout" {
			require.NotEmpty(t, c.Args)
			return c.Args[0]
		}
	}
	t.Fatal("the keycloak Deployment has no init container theme-layout")
	return ""
}

// properties reads a Java properties file as Keycloak reads a theme's
// messages: key=value lines, # and ! comments, and backslash escapes. A line
// that goes on in the next one fails here rather than reading wrong.
func properties(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimLeft(line, " \t")
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		require.False(t, strings.HasSuffix(line, `\`), "a line that goes on in the next: %q", line)
		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "not key=value: %q", line)
		key = strings.TrimSpace(key)
		require.NotContains(t, key, " ", "a key with a space: %q", line)
		var b strings.Builder
		for i := 0; i < len(value); i++ {
			if value[i] != '\\' || i+1 == len(value) {
				b.WriteByte(value[i])
				continue
			}
			i++
			switch value[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				require.LessOrEqual(t, i+5, len(value), "a short \\u escape: %q", line)
				r, err := strconv.ParseUint(value[i+1:i+5], 16, 32)
				require.NoError(t, err, line)
				b.WriteRune(rune(r))
				i += 4
			default:
				b.WriteByte(value[i])
			}
		}
		out[key] = b.String()
	}
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// thenBranch is what a FreeMarker template shows when the directive open
// holds: from it to its own <#else>, <#elseif> or </#if>, nested directives
// passed over.
func thenBranch(t *testing.T, template, open string) string {
	t.Helper()
	start := strings.Index(template, open)
	require.GreaterOrEqual(t, start, 0, "no %s", open)
	rest := template[start+len(open):]
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch {
		case strings.HasPrefix(rest[i:], "<#if "):
			depth++
		case strings.HasPrefix(rest[i:], "</#if>"):
			if depth == 0 {
				return rest[:i]
			}
			depth--
		case depth == 0 && (strings.HasPrefix(rest[i:], "<#else>") || strings.HasPrefix(rest[i:], "<#elseif ")):
			return rest[:i]
		}
	}
	t.Fatalf("%s is never closed", open)
	return ""
}

// themeProfiles are the bundled identity provider's installs: from the
// resource through the operator, the demo's, with mail, and from the chart's
// values alone.
func themeProfiles(t *testing.T) map[string][]*unstructured.Unstructured {
	return map[string][]*unstructured.Unstructured{
		"operator":     renderCR(t, base("zaentrum")),
		"demo":         renderCR(t, demoCR("zaentrum-demo")),
		"mail":         renderCR(t, mailCR(zaentrumv1alpha1.MailSpec{Host: "smtp.example.org", From: "noreply@example.org"})),
		"helm install": helmRender(t, nil),
	}
}

// R.C5: the realm Job switches the bundled realm's password reset on while
// the realm's mail settings name a server and off while they name none, read
// after the Job has written them, or cleared them, as spec.mail says; so the
// sign-in page never offers a link that nothing could send. Before it switches
// the reset on it gives a realm without an email theme the chart's, whose reset
// mail has the platform's words. The switch follows the realm alone, never
// the mail server's user or its Secret.
func TestRC5TheRealmJobSwitchesThePasswordResetWithTheRealmsMail(t *testing.T) {
	for name, z := range map[string]*zaentrumv1alpha1.Zaentrum{
		"no mail": base("zaentrum"),
		"mail": mailCR(zaentrumv1alpha1.MailSpec{
			Host: "smtp.example.org", From: "noreply@example.org", SecretName: "mail-x7k2p",
		}),
		"demo": demoCR("zaentrum-demo"),
	} {
		env := envByName(realmJob(t, z).Spec.Template.Spec.Containers[0])
		assert.Equal(t, "zaentrum", env["EMAIL_THEME"].Value, "%s: a realm without an email theme gets the chart's", name)
		assert.Nil(t, env["EMAIL_THEME"].ValueFrom, name)
	}
	_, hooks := SplitHooks(helmRender(t, nil))
	require.NotNil(t, RealmJob(hooks))
	containers, _, _ := unstructured.NestedSlice(RealmJob(hooks).Object, "spec", "template", "spec", "containers")
	helmEnv := map[string]string{}
	for _, e := range containers[0].(map[string]interface{})["env"].([]interface{}) {
		em := e.(map[string]interface{})
		if v, ok := em["value"].(string); ok {
			helmEnv[em["name"].(string)] = v
		}
	}
	assert.Equal(t, "zaentrum", helmEnv["EMAIL_THEME"], "helm install: the same theme")

	// The script, as the Job runs it.
	args := realmJob(t, base("zaentrum")).Spec.Template.Spec.Containers[0].Args
	require.NotEmpty(t, args)
	script := args[0]
	written := strings.Index(script, `printf '{"smtpServer":{%s}}' "$body" | kc update "realms/$realm" -f - -m`)
	cleared := strings.Index(script, `kc update "realms/$realm" -s 'smtpServer={}'`)
	read := strings.Index(script, `have=$(kc get "realms/$realm" --fields 'smtpServer(*),resetPasswordAllowed,emailTheme')`)
	// Asked for smtpServer alone, in a list or not, the kcadm of Keycloak
	// 26.0.7 answers an empty map: the step would see no server, ever, and
	// switch the reset off on every realm.
	assert.NotRegexp(t, regexp.MustCompile(`--fields '?(?:[^' \n]*,)?smtpServer(?:[,' \n)]|$)`), script,
		"every read of smtpServer asks for smtpServer(*)")
	theme := strings.Index(script, `kc update "realms/$realm" -s "emailTheme=$EMAIL_THEME"`)
	reset := strings.Index(script, `kc update "realms/$realm" -s "resetPasswordAllowed=$reset_want"`)
	console := strings.Index(script, `kc update realms/master -s "attributes.frontendUrl=$MASTER_FRONTEND_URL"`)
	for what, at := range map[string]int{
		"writes the mail settings": written, "clears them": cleared,
		"reads the realm's mail, reset and email theme": read, "sets the email theme": theme,
		"switches the password reset": reset, "moves where the console signs in": console,
	} {
		require.Positive(t, at, "the Job %s", what)
	}
	assert.Less(t, written, read, "the realm is read after its mail settings are written")
	assert.Less(t, cleared, read, "and after they are cleared")
	assert.Less(t, read, theme)
	assert.Less(t, theme, reset, "the reset mail has the platform's words before the reset is on")
	assert.Less(t, reset, console, "before the console's sign-in moves, which ends the Job's token")

	end := strings.Index(script[read:], "\ndemo=\"\"")
	require.Positive(t, end, "the demo user's step follows")
	step := script[read : read+end]
	// On by a server in the realm's mail settings, as kcadm prints one:
	// "host" : "smtp.example.org"; an empty host is no server.
	assert.Contains(t, step, `grep -q '"host" *: *"[^"]' <<<"$have"`)
	assert.Contains(t, step, `grep -q '"resetPasswordAllowed" *: *true' <<<"$have"`)
	assert.Contains(t, step, `grep -q '"emailTheme" *: *"[^"]' <<<"$have"`, "an email theme someone chose stays")
	for _, never := range []string{"MAIL_USERNAME", "MAIL_USER", "MAIL_PASSWORD", "MAIL_SECRET", "MAIL_HOST"} {
		assert.NotContains(t, step, never, "the reset follows the realm's own settings, not %s", never)
	}
	// Each write is kcadm's merge: a body put on its own sets the device code,
	// CIBA and PAR settings back to their defaults (Keycloak 26.0.7). What the
	// step does is held by the tests that run it (resetrun_test.go).
	assert.NotContains(t, step, "-f -", "no body put on its own")
	assert.Contains(t, script, `[ -z "$theme" ] || summary+="${summary:+; }$theme"`)
	assert.Contains(t, script, `[ -z "$reset" ] || summary+="${summary:+; }$reset"`)
	// What the run says, in the words of the chapter (Words, the realm job's
	// summary and a failed run of the realm job).
	for _, said := range []string{
		`reset="the sign-in page offers a password reset now"`,
		`reset="the sign-in page offers no password reset now: the realm sends no mail"`,
		`theme="the realm's email theme is $EMAIL_THEME now"`,
		`fail "cannot read realm $realm: $(why)"`,
		`fail "cannot set the email theme of realm $realm: $(why)"`,
		`fail "cannot switch the password reset of realm $realm: $(why)"`,
	} {
		assert.Contains(t, step, said)
	}
}

// R.C5: the chart's sign-in page shows “Forgot password?” while the realm
// allows a password reset and, without one, under the form: “Forgot your
// password? Ask an admin for a new invite link.” The realm's switch decides,
// at sign-in: the page is the same with mail and without.
func TestRC5TheSignInPageSaysWhomToAskWithoutAPasswordReset(t *testing.T) {
	const sentence = "Forgot your password? Ask an admin for a new invite link."
	var first map[string]string
	for name, objs := range themeProfiles(t) {
		files := themeFiles(t, objs)
		login := files["login.ftl"]
		require.NotEmpty(t, login, name)

		assert.Contains(t, login, `<#if (realm.resetPasswordAllowed)!false || (realm.registrationAllowed)!false>`, name)
		with := thenBranch(t, login, `<#if (realm.resetPasswordAllowed)!false>`)
		assert.Contains(t, with, `href="${url.loginResetCredentialsUrl}">Forgot password?</a>`, "%s: with the reset, the link", name)
		assert.NotContains(t, with, sentence, name)

		without := thenBranch(t, login, `<#if !((realm.resetPasswordAllowed)!false)>`)
		assert.Contains(t, without, `<p class="note">`+sentence+`</p>`, "%s: without it, the sentence", name)
		assert.NotContains(t, without, "Forgot password?", "%s: and no link", name)
		assert.NotContains(t, without, "<a ", name)
		assert.Equal(t, 1, strings.Count(login, sentence), name)
		assert.Less(t, strings.Index(login, "</form>"), strings.Index(login, sentence), "%s: under the form", name)
		assert.Regexp(t, regexp.MustCompile(`(?m)^\.note \{`), files["zaentrum.css"], "%s: the sentence is styled", name)

		if first == nil {
			first = files
		} else {
			assert.Equal(t, first, files, "%s: one theme for every install", name)
		}
	}

	ext := base("zaentrum")
	ext.Spec.Identity.Mode = zaentrumv1alpha1.IdentityExternal
	ext.Spec.Identity.Issuer = "https://sso.example.org/realms/example"
	assert.Nil(t, find(t, renderCR(t, ext), "ConfigMap", "keycloak-theme"), "an external provider has its own sign-in page")
}

// R.C5: the chart's theme gives the password reset mail the words of the
// chapter's Words, The mails: an email part beside the sign-in page, laid out
// where Keycloak looks for it, that keeps Keycloak's own mails but for the
// reset's subject and text — in the plain text part and the HTML part alike,
// so that no mail program shows Keycloak's words instead. The realm Job names
// it as the realm's email theme.
//
// Keycloak 26.0.7's base password-reset templates, text and HTML, fill both in
// as msg(key, link, linkExpiration, realmName, linkExpirationFormatter(
// linkExpiration)): {link} is {0}, and {time}, the realm's time for the link as
// Keycloak words it (“5 minutes” today), is {3}.
func TestRC5TheResetMailHasThePlatformsWords(t *testing.T) {
	words := []string{
		"Somebody asked to reset the password of your account.",
		"If it was you, open this link and choose a new password:",
		"{link}",
		"The link works for {time}.",
		"If it was not you, ignore this mail. Your password stays as it is.",
	}
	keycloak := strings.NewReplacer("{link}", "{0}", "{time}", "{3}")
	wantText := keycloak.Replace(strings.Join(words, "\n\n"))
	wantHTML := ""
	for _, w := range words {
		if w == "{link}" {
			wantHTML += `<p><a href="{0}">{0}</a></p>`
		} else {
			wantHTML += "<p>" + keycloak.Replace(w) + "</p>"
		}
	}
	placeholder := regexp.MustCompile(`\{[^}]*\}`)

	for name, objs := range themeProfiles(t) {
		files := themeFiles(t, objs)
		require.Contains(t, files, "email-theme.properties", name)
		require.Contains(t, files, "email-messages_en.properties", name)
		theme := properties(t, files["email-theme.properties"])
		assert.Equal(t, map[string]string{"parent": "keycloak"}, theme,
			"%s: every other mail as Keycloak's own email theme has it, the one a realm has by default", name)

		msgs := properties(t, files["email-messages_en.properties"])
		assert.Equal(t, []string{"passwordResetBody", "passwordResetBodyHtml", "passwordResetSubject"}, keysOf(msgs),
			"%s: the reset mail's subject, text and HTML, and no other mail's", name)
		assert.Equal(t, "Reset your password", msgs["passwordResetSubject"], name)
		// Keycloak fills them in with Java's MessageFormat: a single quote
		// would vanish, and a brace but the link's and the time's would be
		// filled with the realm's id, or the time's bare number.
		for key, v := range msgs {
			assert.NotContains(t, v, "'", "%s: %s", name, key)
			for _, p := range placeholder.FindAllString(v, -1) {
				assert.Contains(t, []string{"{0}", "{3}"}, p, "%s: %s fills in the link and the time alone", name, key)
			}
		}
		assert.Equal(t, wantText, msgs["passwordResetBody"], "%s: the text part", name)
		assert.Equal(t, wantHTML, msgs["passwordResetBodyHtml"], "%s: the HTML part, the same words", name)
		assert.Equal(t, 1, strings.Count(msgs["passwordResetBody"], "{3}"), "%s: the text part names the time once", name)
		assert.Equal(t, 1, strings.Count(msgs["passwordResetBodyHtml"], "{3}"), "%s: the HTML part names the time once", name)
		assert.NotContains(t, msgs["passwordResetBody"]+msgs["passwordResetBodyHtml"], "5 minutes",
			"%s: the time is the realm's, never written down", name)

		// Laid out as Keycloak reads a folder theme: <theme>/email/theme.properties
		// and <theme>/email/messages/messages_en.properties, beside
		// <theme>/login, under the name the realm Job gives the realm.
		layout := themeLayout(t, objs)
		dirs := regexp.MustCompile(`(?m)^\s*E=/themes/([a-z0-9.-]+)/email$`).FindStringSubmatch(layout)
		require.Len(t, dirs, 2, "%s: the email part's folder", name)
		assert.Equal(t, "zaentrum", dirs[1], name)
		assert.Contains(t, layout, "D=/themes/"+dirs[1]+"/login", "%s: beside the sign-in page", name)
		assert.Contains(t, layout, `mkdir -p "$E/messages"`, name)
		assert.Regexp(t, regexp.MustCompile(`(?m)^\s*cp /src/email-theme\.properties +"\$E/theme\.properties"$`), layout, name)
		assert.Regexp(t, regexp.MustCompile(`(?m)^\s*cp /src/email-messages_en\.properties +"\$E/messages/messages_en\.properties"$`), layout, name)
		for file := range files {
			assert.Contains(t, layout, "/src/"+file+" ", "%s: %s is laid out", name, file)
		}
	}
	env := envByName(realmJob(t, base("zaentrum")).Spec.Template.Spec.Containers[0])
	assert.Equal(t, "zaentrum", env["EMAIL_THEME"].Value, "the realm's email theme is the one laid out")
	assert.Equal(t, "zaentrum", realmImport(t, renderCR(t, base("zaentrum")))["loginTheme"], "the theme the realm signs in with")
}

// R.C5: the password reset mail carries no picture and no address but its link
// (Mail, What is sent): its HTML part has no image and nothing it would load
// from elsewhere, one link, {0}, and no other URL or e-mail address; neither
// has its subject or its text part.
func TestRC5TheResetMailsHTMLHoldsNoPictureAndNoAddressButTheLink(t *testing.T) {
	url := regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|mailto:|www\.)`)
	address := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	href := regexp.MustCompile(`(?i)\bhref\s*=\s*"([^"]*)"`)
	for name, objs := range themeProfiles(t) {
		msgs := properties(t, themeFiles(t, objs)["email-messages_en.properties"])
		html := msgs["passwordResetBodyHtml"]
		require.NotEmpty(t, html, name)
		for _, never := range []string{"<img", "src=", "url("} {
			assert.NotContains(t, strings.ToLower(html), never, "%s: no picture, and nothing loaded from elsewhere", name)
		}
		hrefs := href.FindAllStringSubmatch(html, -1)
		require.Len(t, hrefs, 1, "%s: one link", name)
		assert.Equal(t, "{0}", hrefs[0][1], "%s: the link Keycloak fills in", name)
		for _, key := range []string{"passwordResetSubject", "passwordResetBody", "passwordResetBodyHtml"} {
			assert.Empty(t, url.FindAllString(msgs[key], -1), "%s: %s names no URL but {0}", name, key)
			assert.Empty(t, address.FindAllString(msgs[key], -1), "%s: %s names no e-mail address", name, key)
		}
	}
}

// R.C5: the chart's realm is made with the password reset off, so a new
// platform offers none before the realm Job's first run (Mail, The settings);
// the Job's run switches it by the realm's mail. The switch is the import's
// alone: no value of the realm Job's environment carries it, so the Job's
// fingerprint does not move with it, and a realm that exists, which is never
// imported again, is brought in step by the Job, as it was.
func TestRC5ANewRealmIsMadeWithThePasswordResetOff(t *testing.T) {
	for name, objs := range themeProfiles(t) {
		assert.Equal(t, false, realmImport(t, objs)["resetPasswordAllowed"], "%s: a new realm offers no reset", name)
		_, hooks := SplitHooks(objs)
		require.NotNil(t, RealmJob(hooks), name)
		containers, _, _ := unstructured.NestedSlice(RealmJob(hooks).Object, "spec", "template", "spec", "containers")
		for _, e := range containers[0].(map[string]interface{})["env"].([]interface{}) {
			em := e.(map[string]interface{})
			if v, ok := em["value"].(string); ok {
				assert.NotContains(t, v, "resetPasswordAllowed", "%s: %s", name, em["name"])
			}
		}
	}
}

// R.C5: every realm import the repository ships makes the realm with the
// password reset off, as the chart's does: deploy/base's, which CI's kind job
// applies, and the one the Keycloak image of platform/keycloak bakes in. No
// realm Job gives those realms mail, so none would ever switch it off.
func TestRC5TheOtherRealmImportsMakeTheResetOffToo(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/base/keycloak-realm.yaml")
	require.NoError(t, err)
	var cm struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &cm))
	imports := map[string]string{"deploy/base/keycloak-realm.yaml": cm.Data["zaentrum-realm.json"]}
	raw, err = os.ReadFile("../../../platform/keycloak/zaentrum-realm.json")
	require.NoError(t, err)
	imports["platform/keycloak/zaentrum-realm.json"] = string(raw)
	for name, body := range imports {
		var realm map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &realm), name)
		assert.Equal(t, "zaentrum", realm["realm"], name)
		assert.Equal(t, false, realm["resetPasswordAllowed"], "%s: a new realm offers no reset", name)
	}
}
