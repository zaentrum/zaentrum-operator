package templates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The realm Job's script (files/realm-config.sh) is run here: its steps, cut
// out of the script as the chart ships it, with bash, against a stand-in for
// kcadm.sh. The stand-in is this test binary itself: run with kcadmDir set, it
// is kcadm, and answers what the steps ask of one realm as the kcadm of
// Keycloak 26.0.7 answered it against the chart's realm:
//
//   - get realms/<realm> --fields <list>: the fields asked for, printed as
//     kcadm prints them, "name" : value; a map asked for by its name alone
//     comes without its keys, name(*) with every one; a password is masked.
//   - update realms/<realm>: with -m, or with -s or -d and no file, a merge:
//     the realm as get answers it, the body merged into it key by key, -s set
//     and -d deleted, and the whole put back. With -f alone, the body alone is
//     put. A put sets what it names: the mail settings (smtpServer) whole, a
//     masked password keeping the one kept, and attributes whole; and it sets
//     back to their defaults what a body leaves out of the device code
//     lifespan and polling interval and of the CIBA and PAR attributes.
//
// It keeps the realm in <dir>/realm.json, with the mail password as it was
// given; logs each call's arguments to calls.log and each write to
// writes.log; and fails a call whose arguments or body hold kcadmFail, saying
// kcadmWhy, in which {body} is the body it was given.
const (
	kcadmDir  = "ZAENTRUM_TEST_KCADM"
	kcadmFail = "ZAENTRUM_TEST_KCADM_FAIL"
	kcadmWhy  = "ZAENTRUM_TEST_KCADM_WHY"
	masked    = "**********"
)

// init makes this binary the stand-in kcadm, before any test runs, when
// kcadmDir is set. The package's TestMain is the offline guard's (main_test.go).
func init() {
	if dir := os.Getenv(kcadmDir); dir != "" {
		os.Exit(fakeKcadm(dir, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// What Keycloak 26.0.7's realm update sets back to its default when a body it
// is given leaves it out, as measured against the chart's realm.
var (
	defaultsOfTop        = map[string]any{"oauth2DeviceCodeLifespan": float64(600), "oauth2DevicePollingInterval": float64(5)}
	defaultsOfAttributes = map[string]any{"cibaBackchannelTokenDeliveryMode": "poll", "cibaExpiresIn": "120",
		"cibaInterval": "5", "cibaAuthRequestedUserHint": "login_hint", "parRequestUriLifespan": "60"}
)

func fakeKcadm(dir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	appendLine(dir, "calls.log", strings.Join(args, " "))
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	args = rest
	if len(args) >= 2 && args[0] == "config" && args[1] == "credentials" {
		return 0
	}
	if len(args) < 2 || !strings.HasPrefix(args[1], "realms/") {
		fmt.Fprintf(stderr, "the stand-in kcadm does not answer %q\n", args)
		return 2
	}
	var body []byte
	if has(args, "-f") {
		body, _ = io.ReadAll(stdin)
	}
	if f := os.Getenv(kcadmFail); f != "" && (strings.Contains(strings.Join(args, " "), f) || bytes.Contains(body, []byte(f))) {
		why := os.Getenv(kcadmWhy)
		if why == "" {
			why = "HTTP error 500"
		}
		fmt.Fprintln(stderr, strings.ReplaceAll(why, "{body}", string(body)))
		return 1
	}
	realm := loadRealm(dir)
	switch args[0] {
	case "get":
		spec := value(args, "--fields")
		if spec == "" {
			fmt.Fprintln(stderr, "the stand-in kcadm answers fields alone")
			return 2
		}
		printFields(stdout, realm, spec)
		return 0
	case "update":
		merge := has(args, "-m") || !has(args, "-f")
		put := map[string]any{}
		if merge {
			put = shown(realm)
		}
		if has(args, "-f") {
			var b map[string]any
			if err := json.Unmarshal(body, &b); err != nil {
				fmt.Fprintf(stderr, "Failed to parse JSON: %v\n", err)
				return 1
			}
			if merge {
				mergeInto(put, b)
			} else {
				put = b
			}
		}
		for _, s := range values(args, "-s") {
			name, v, _ := strings.Cut(s, "=")
			var parsed any
			if err := json.Unmarshal([]byte(v), &parsed); err != nil {
				parsed = v
			}
			setPath(put, name, parsed)
		}
		for _, d := range values(args, "-d") {
			deletePath(put, d)
		}
		before := shown(realm)
		applyPut(realm, put)
		saveRealm(dir, realm)
		kind := "put"
		if merge {
			kind = "merge"
		}
		appendLine(dir, "writes.log", kind+" "+strings.Join(changedKeys(before, shown(realm)), ","))
		return 0
	}
	fmt.Fprintf(stderr, "the stand-in kcadm does not answer %q\n", args)
	return 2
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func value(args []string, flag string) string {
	v := values(args, flag)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func values(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

func appendLine(dir, name, line string) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func loadRealm(dir string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(dir, "realm.json"))
	if err != nil {
		panic(err)
	}
	var realm map[string]any
	if err := json.Unmarshal(raw, &realm); err != nil {
		panic(err)
	}
	return realm
}

func saveRealm(dir string, realm map[string]any) {
	raw, err := json.Marshal(realm)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "realm.json"), raw, 0o600); err != nil {
		panic(err)
	}
}

// shown is the realm as get answers it: a deep copy, the mail password masked.
func shown(realm map[string]any) map[string]any {
	out := deepCopy(realm).(map[string]any)
	if smtp, ok := out["smtpServer"].(map[string]any); ok && smtp["password"] != nil {
		smtp["password"] = masked
	}
	return out
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopy(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopy(vv)
		}
		return out
	}
	return v
}

// mergeInto merges b into a as kcadm's merge does: a map key by key, any
// other value replaced.
func mergeInto(a, b map[string]any) {
	for k, v := range b {
		if am, ok := a[k].(map[string]any); ok {
			if bm, ok := v.(map[string]any); ok {
				mergeInto(am, bm)
				continue
			}
		}
		a[k] = deepCopy(v)
	}
}

func setPath(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, parts[len(parts)-1])
}

// applyPut is Keycloak's realm update of a body put.
func applyPut(realm, put map[string]any) {
	for k, v := range put {
		switch k {
		case "smtpServer":
			next, _ := deepCopy(v).(map[string]any)
			if next == nil {
				next = map[string]any{}
			}
			if next["password"] == masked {
				if old, ok := realm["smtpServer"].(map[string]any); ok && old["password"] != nil {
					next["password"] = old["password"]
				} else {
					delete(next, "password")
				}
			}
			realm["smtpServer"] = next
		default:
			realm[k] = deepCopy(v)
		}
	}
	for k, def := range defaultsOfTop {
		if _, ok := put[k]; !ok {
			realm[k] = def
		}
	}
	attrs, _ := realm["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
		realm["attributes"] = attrs
	}
	given, _ := put["attributes"].(map[string]any)
	for k, def := range defaultsOfAttributes {
		if _, ok := given[k]; !ok {
			attrs[k] = def
		}
	}
}

func changedKeys(a, b map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			ja, _ := json.Marshal(a[k])
			jb, _ := json.Marshal(b[k])
			if !bytes.Equal(ja, jb) {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// printFields prints the fields spec names as kcadm prints them.
func printFields(w io.Writer, realm map[string]any, spec string) {
	var items []string
	depth, cur := 0, ""
	for _, r := range spec {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		}
		if r == ',' && depth == 0 {
			items = append(items, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	items = append(items, cur)
	show := shown(realm)
	var lines []string
	for _, item := range items {
		name, rest, _ := strings.Cut(item, "(")
		v, ok := show[name]
		if !ok || v == nil {
			continue
		}
		if m, isMap := v.(map[string]any); isMap && rest != "*)" {
			v = map[string]any{}
			_ = m
		}
		lines = append(lines, "  "+quote(name)+" : "+pretty(v, "  "))
	}
	fmt.Fprintln(w, "{\n"+strings.Join(lines, ",\n")+"\n}")
}

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func pretty(v any, indent string) string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return "{ }"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, indent+"  "+quote(k)+" : "+pretty(t[k], indent+"  "))
		}
		return "{\n" + strings.Join(parts, ",\n") + "\n" + indent + "}"
	case string:
		return quote(t)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// realmRun is one realm the stand-in keeps, and the script's steps run over
// it.
type realmRun struct {
	t   *testing.T
	dir string
}

// newRealmRun is a realm as seed has it, the mail password as it was given.
func newRealmRun(t *testing.T, seed map[string]any) *realmRun {
	t.Helper()
	r := &realmRun{t: t, dir: t.TempDir()}
	r.set(seed)
	return r
}

func (r *realmRun) set(realm map[string]any) {
	r.t.Helper()
	saveRealm(r.dir, deepCopy(realm).(map[string]any))
	require.NoError(r.t, os.WriteFile(filepath.Join(r.dir, "writes.log"), nil, 0o600))
}

// realm is the realm as the stand-in keeps it, the password as given.
func (r *realmRun) realm() map[string]any { return loadRealm(r.dir) }

func (r *realmRun) lines(name string) []string {
	raw, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// writes are the writes since the last set, one a line: put or merge, and the
// realm's fields they changed.
func (r *realmRun) writes() []string { return r.lines("writes.log") }

// stepRun is what one run of the steps did.
type stepRun struct {
	exit           int
	stdout, stderr string
	termination    string
	said           map[string]string
}

// run runs the script's steps over the realm with bash, the environment env
// besides the Job's own; it prints the steps' summaries as NAME=value.
func (r *realmRun) run(steps []string, summaries []string, env map[string]string) stepRun {
	r.t.Helper()
	bash, err := exec.LookPath("bash")
	require.NoError(r.t, err, "the script's steps run with bash")
	self, err := os.Executable()
	require.NoError(r.t, err)
	var src strings.Builder
	src.WriteString("set -euo pipefail\nset -f\n")
	src.WriteString(scriptPart(r.t, "setup"))
	for _, h := range []string{"fail", "kc", "why", "js"} {
		src.WriteString(scriptPart(r.t, h))
	}
	for _, s := range steps {
		src.WriteString(scriptPart(r.t, s))
	}
	for _, s := range summaries {
		fmt.Fprintf(&src, "printf '%s=%%s\\n' \"$%s\"\n", strings.ToUpper(s), s)
	}
	termlog := filepath.Join(r.dir, "termlog")
	_ = os.Remove(termlog)
	cmd := exec.Command(bash, "-c", src.String())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + r.dir, "TMPDIR=" + r.dir,
		kcadmDir + "=" + r.dir, "KCADM=" + self, "REALM=zaentrum", "TERMINATION_LOG=" + termlog}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	out := stepRun{stdout: stdout.String(), stderr: stderr.String(), said: map[string]string{}}
	if ee, ok := err.(*exec.ExitError); ok {
		out.exit = ee.ExitCode()
	} else {
		require.NoError(r.t, err)
	}
	if raw, err := os.ReadFile(termlog); err == nil {
		out.termination = string(raw)
	}
	for _, l := range strings.Split(out.stdout, "\n") {
		for _, s := range summaries {
			if v, ok := strings.CutPrefix(l, strings.ToUpper(s)+"="); ok {
				out.said[s] = v
			}
		}
	}
	return out
}

// scriptPart is a part of the realm script as the chart ships it: setup, the
// lines from realm= to the error file; a helper, fail, kc, why or js; or a
// step, from its first comment line to the line before the next step's.
func scriptPart(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../platform/chart/files/realm-config.sh")
	require.NoError(t, err)
	lines := strings.Split(string(raw), "\n")
	index := func(from int, prefixes ...string) int {
		for i := from; i < len(lines); i++ {
			for _, p := range prefixes {
				if strings.HasPrefix(lines[i], p) {
					return i
				}
			}
		}
		t.Fatalf("realm-config.sh has no line starting %q after line %d", prefixes, from)
		return -1
	}
	var start, end int
	switch name {
	case "setup":
		start = index(0, "realm=${REALM:-zaentrum}")
		end = index(start, `: >"$err"`) + 1
	case "fail", "why", "js":
		start = index(0, name+"() {")
		end = index(start, "}") + 1
	case "kc":
		start = index(0, "kc() ")
		end = start + 1
	case "mail":
		start = index(0, "# The realm's mail settings: the platform's mail server, or none.")
		end = index(start+1, "# The realm's password reset follows", `demo=""`)
	case "reset":
		start = index(0, "# The realm's password reset follows")
		end = index(start+1, `demo=""`)
	default:
		t.Fatalf("no part %q", name)
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}
