package templates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The measuring Jobs' script (files/measure-volume.sh) is run here, whole, as
// the chart ships it, with bash, over folders of the test's own, against
// stand-ins for stat, psql and pg_isready. The stand-ins are this test binary
// itself: run with measureDir set, it is the tool its name says:
//
//   - stat -f -c '%S %b %a %i' PATH: the line <dir>/stat.json gives for PATH,
//     or a failure where it gives none;
//   - pg_isready: exit with the code in <dir>/ready, 0 where there is none;
//   - psql: the size query, read from its input, takes the databases' names
//     as the values -v gives it, each by its key (:'chino'), and is refused
//     where it does not; it answers each key with the size <dir>/databases.json
//     gives the name, or none. Each call's statements go to <dir>/psql.log, and
//     its values apart to <dir>/psql-values.log. The data directory is
//     <dir>/datadir; the COPY of the volume of the data runs the program the
//     SQL builds, as the server runs it, with /bin/sh and this PATH, or fails
//     saying <dir>/copy-error, as verbose psql says it;
//   - find, when the case puts one in the PATH: a failure.
const measureDir = "ZAENTRUM_TEST_MEASURE"

// init makes this binary a stand-in, before any test runs, when measureDir is
// set.
func init() {
	if dir := os.Getenv(measureDir); dir != "" {
		os.Exit(fakeMeasureTool(dir, filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr))
	}
}

func fakeMeasureTool(dir, tool string, args []string, stdout, stderr io.Writer) int {
	read := func(name string) (string, bool) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		return string(raw), err == nil
	}
	switch tool {
	case "stat":
		if len(args) != 4 || args[0] != "-f" || args[1] != "-c" || args[2] != "%S %b %a %i" {
			fmt.Fprintf(stderr, "the stand-in stat does not answer %q\n", args)
			return 2
		}
		raw, _ := read("stat.json")
		lines := map[string]string{}
		_ = json.Unmarshal([]byte(raw), &lines)
		line, ok := lines[filepath.Clean(args[3])]
		if !ok {
			fmt.Fprintf(stderr, "stat: can't read file system information for '%s': No such file or directory\n", args[3])
			return 1
		}
		fmt.Fprintln(stdout, line)
		return 0
	case "pg_isready":
		if code, ok := read("ready"); ok && strings.TrimSpace(code) != "0" {
			return 2
		}
		return 0
	case "psql":
		var commands []string
		values := map[string]string{}
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "-c" && i+1 < len(args):
				commands = append(commands, args[i+1])
				i++
			case args[i] == "-v" && i+1 < len(args):
				k, v, _ := strings.Cut(args[i+1], "=")
				values[k] = v
				i++
			}
		}
		if len(commands) == 0 {
			input, _ := io.ReadAll(os.Stdin)
			commands = []string{string(input)}
		}
		all := strings.Join(commands, "\n")
		appendLine(dir, "psql.log", strings.ReplaceAll(strings.Join(commands, " ;; "), "\n", " "))
		raw, _ := json.Marshal(values)
		appendLine(dir, "psql-values.log", string(raw))
		switch {
		case strings.Contains(all, "pg_database_size"):
			sizes := map[string]int64{}
			text, _ := read("databases.json")
			_ = json.Unmarshal([]byte(text), &sizes)
			for _, key := range []string{"chino", "katalog", "keycloak", "portal"} {
				if !strings.Contains(all, ":'"+key+"'") {
					fmt.Fprintf(stderr, "ERROR:  42601: the statement does not take %s as a value\n", key)
					return 3
				}
				if n, ok := sizes[values[key]]; ok {
					fmt.Fprintf(stdout, "%s %d\n", key, n)
				} else {
					fmt.Fprintf(stdout, "%s \n", key)
				}
			}
			return 0
		case strings.TrimSpace(all) == "SELECT current_setting('data_directory')":
			datadir, _ := read("datadir")
			fmt.Fprintln(stdout, strings.TrimSpace(datadir))
			return 0
		case strings.Contains(all, "FROM PROGRAM"):
			if why, ok := read("copy-error"); ok {
				fmt.Fprint(stderr, why)
				return 3
			}
			datadir, _ := read("datadir")
			datadir = strings.TrimSpace(datadir)
			program, err := copyProgram(all, datadir)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			cmd := exec.Command("/bin/sh", "-c", program)
			cmd.Env = os.Environ()
			out, err := cmd.Output()
			if err != nil {
				fmt.Fprintf(stderr, "ERROR:  program %q failed\n", program)
				return 3
			}
			fmt.Fprint(stdout, string(out))
			return 0
		}
		fmt.Fprintf(stderr, "the stand-in psql does not answer %q\n", commands)
		return 2
	case "find":
		fmt.Fprintln(stderr, "find: the stand-in fails")
		return 1
	}
	fmt.Fprintf(stderr, "no stand-in %s\n", tool)
	return 2
}

// copyProgram is the program a COPY ... FROM PROGRAM of the script's makes the
// server run: the SQL concatenation it gives format's %L, its literals
// unquoted, current_setting('data_directory') the data directory.
func copyProgram(sql, datadir string) (string, error) {
	m := regexp.MustCompile(`format\('COPY pg_temp\.measure FROM PROGRAM %L', (.*)\); END`).FindStringSubmatch(sql)
	if m == nil {
		return "", fmt.Errorf("the stand-in psql finds no COPY pg_temp.measure FROM PROGRAM in %q", sql)
	}
	var program strings.Builder
	for _, part := range strings.Split(m[1], " || ") {
		switch {
		case part == "current_setting('data_directory')":
			program.WriteString(datadir)
		case len(part) >= 2 && strings.HasPrefix(part, "'") && strings.HasSuffix(part, "'"):
			program.WriteString(strings.ReplaceAll(part[1:len(part)-1], "''", "'"))
		default:
			return "", fmt.Errorf("the stand-in psql cannot read %q of %q", part, m[1])
		}
	}
	return program.String(), nil
}

// measureRun is one run of the script: its exit code, its log and its
// report.
type measureRunResult struct {
	exit           int
	stdout, stderr string
	raw            string
	report         map[string]any
}

// measureEnv is the stand-ins' state and the folders a case measures.
type measureEnv struct {
	t     *testing.T
	dir   string // the stand-ins' state
	bin   string // the stand-ins, ahead of PATH
	stats map[string]string
}

func newMeasureEnv(t *testing.T, tools ...string) *measureEnv {
	t.Helper()
	e := &measureEnv{t: t, dir: t.TempDir(), bin: t.TempDir(), stats: map[string]string{}}
	self, err := os.Executable()
	require.NoError(t, err)
	for _, tool := range append([]string{"stat", "psql", "pg_isready"}, tools...) {
		require.NoError(t, os.Symlink(self, filepath.Join(e.bin, tool)))
	}
	return e
}

// claim is a folder of the test's, measured as the stand-in stat says.
func (e *measureEnv) claim(stat string) string {
	e.t.Helper()
	dir := filepath.Join(e.t.TempDir(), "claim")
	require.NoError(e.t, os.Mkdir(dir, 0o755))
	if stat != "" {
		e.stat(dir, stat)
	}
	return dir
}

func (e *measureEnv) stat(path, line string) {
	e.t.Helper()
	e.stats[filepath.Clean(path)] = line
	raw, err := json.Marshal(e.stats)
	require.NoError(e.t, err)
	require.NoError(e.t, os.WriteFile(filepath.Join(e.dir, "stat.json"), raw, 0o600))
}

func (e *measureEnv) write(name, content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(filepath.Join(e.dir, name), []byte(content), 0o600))
}

// run runs the script as the Job does, with the environment env.
func (e *measureEnv) run(env map[string]string) measureRunResult {
	e.t.Helper()
	bash, err := exec.LookPath("bash")
	require.NoError(e.t, err, "the script runs with bash")
	script, err := os.ReadFile("../../platform/chart/files/measure-volume.sh")
	require.NoError(e.t, err)
	termlog := filepath.Join(e.dir, "termlog")
	_ = os.Remove(termlog)
	cmd := exec.Command(bash, "-c", string(script))
	cmd.Env = []string{"PATH=" + e.bin + ":" + os.Getenv("PATH"), "HOME=" + e.dir, "TMPDIR=" + e.dir,
		measureDir + "=" + e.dir, "TERMINATION_LOG=" + termlog}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	out := measureRunResult{stdout: stdout.String(), stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		out.exit = ee.ExitCode()
	} else {
		require.NoError(e.t, err)
	}
	raw, err := os.ReadFile(termlog)
	require.NoError(e.t, err, "the script always leaves a report: %s", out.stderr)
	out.raw = string(raw)
	require.LessOrEqual(e.t, len(raw), 4096, "the report fits what the kubelet keeps")
	require.NoError(e.t, json.Unmarshal(raw, &out.report), "the report is JSON: %s", raw)
	return out
}

// set is a set's directory on a claim, with the files named, each holding
// its content.
func set(t *testing.T, claim, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(claim, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for f, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600))
	}
}

// listed is a report's sets as [name, version, whole, own], own "" for null.
func listed(t *testing.T, rep map[string]any, key string) [][4]any {
	t.Helper()
	var out [][4]any
	one := func(v any) {
		item, ok := v.([]any)
		require.True(t, ok, "%s is a list: %v", key, v)
		require.Len(t, item, 5)
		bytes, _ := item[1].(float64)
		assert.Greater(t, bytes, float64(0), "a set's size: %v", item)
		assert.Zero(t, int64(bytes)%1024, "du's kilobytes, as bytes: %v", item)
		own := any("")
		if item[4] != nil {
			own = item[4]
		}
		out = append(out, [4]any{item[0], item[2], item[3], own})
	}
	switch v := rep[key].(type) {
	case nil:
	case []any:
		if key == "set" {
			one(v)
			break
		}
		for _, item := range v {
			one(item)
		}
	}
	return out
}

func TestRC7MeasureScriptMeasuresAClaim(t *testing.T) {
	t.Run("R.C7 a claim: the filesystem's size, what an ordinary writer can use of it, and its id", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b3c4d5e6f7a8b")
		out := e.run(map[string]string{"MEASURE_DIR": claim})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Equal(t, `{"v":1,"size":4096000,"free":1024000,"fs":"1a2b3c4d5e6f7a8b"}`, out.raw)
		assert.Contains(t, out.stdout, "measure: "+claim+": 4096000 bytes, 1024000 free, filesystem 1a2b3c4d5e6f7a8b\n")
	})

	t.Run("R.C7 no folder to read", func(t *testing.T) {
		e := newMeasureEnv(t)
		missing := filepath.Join(t.TempDir(), "nothing")
		out := e.run(map[string]string{"MEASURE_DIR": missing})
		assert.Equal(t, 1, out.exit)
		assert.Equal(t, missing+" is not a directory this measuring can read", out.report["failed"])
		assert.NotContains(t, out.report, "size")
	})

	t.Run("R.C7 a filesystem stat cannot read", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("")
		out := e.run(map[string]string{"MEASURE_DIR": claim})
		assert.Equal(t, 1, out.exit)
		assert.Equal(t, "cannot read the filesystem at "+claim, out.report["failed"])

		e.stat(claim, "4096 lots 250 1a2b")
		out = e.run(map[string]string{"MEASURE_DIR": claim})
		assert.Equal(t, "cannot read the filesystem at "+claim, out.report["failed"], "a line that holds no numbers")
	})
}

// libraryEnv is the library's Job's environment over a claim, with the bundled
// Postgres keeping its data on a claim.
func libraryEnv(claim string) map[string]string {
	return map[string]string{"MEASURE_DIR": claim, "PGHOST": "postgres", "PGUSER": "zaentrum", "PGPASSWORD": "secret",
		"DB_CHINO": "chino", "DB_KATALOG": "katalog", "DB_KEYCLOAK": "keycloak", "DB_PORTAL": "portal",
		"POSTGRES_VOLUME": "true", "PG_WAIT_SECONDS": "0"}
}

// sizes are what the stand-in Postgres answers for each database's name.
func sizes(t *testing.T, e *measureEnv, byName map[string]int64) {
	t.Helper()
	raw, err := json.Marshal(byName)
	require.NoError(t, err)
	e.write("databases.json", string(raw))
}

var defaultSizes = map[string]int64{"chino": 7635471, "katalog": 9000000, "keycloak": 12000000, "portal": 8000000}

func TestRC7MeasureScriptAsksThePostgres(t *testing.T) {
	const datadir = "/var/lib/postgresql/data/pgdata"

	t.Run("R.C7 the databases' sizes by their key, and the volume of their data as the server reads it", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		sizes(t, e, defaultSizes)
		e.write("datadir", datadir)
		e.stat(datadir, "4096 2500 2000 99ff")
		out := e.run(libraryEnv(claim))
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Equal(t, `{"v":1,"size":4096000,"free":1024000,"fs":"1a2b",`+
			`"databases":{"chino":7635471,"katalog":9000000,"keycloak":12000000,"portal":8000000},`+
			`"postgres":{"size":10240000,"free":8192000,"fs":"99ff"}}`, out.raw)
		assert.Contains(t, out.stdout, "measure: the databases take 36635471 bytes\n")
		assert.Contains(t, out.stdout, "measure: "+datadir+": 10240000 bytes, 8192000 free, filesystem 99ff\n")
		assert.Contains(t, strings.Join(lines(t, e.dir, "psql.log"), "\n"), "COPY pg_temp.measure FROM PROGRAM %L",
			"the server runs stat itself, for its superuser")
	})

	t.Run("R.C7 the databases' names reach psql as values it quotes, each whole", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		names := map[string]string{"DB_CHINO": "chi'no", "DB_KATALOG": "kat alog", "DB_KEYCLOAK": "key;cloak", "DB_PORTAL": "portal"}
		sizes(t, e, map[string]int64{"chi'no": 1, "kat alog": 2, "key;cloak": 3, "portal": 4})
		env := libraryEnv(claim)
		for k, v := range names {
			env[k] = v
		}
		delete(env, "POSTGRES_VOLUME")
		out := e.run(env)
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Equal(t, map[string]any{"chino": float64(1), "katalog": float64(2), "keycloak": float64(3), "portal": float64(4)},
			out.report["databases"], "by their key, whatever they are called")
		statements := strings.Join(lines(t, e.dir, "psql.log"), "\n")
		for _, name := range []string{"chi'no", "kat alog", "key;cloak"} {
			assert.NotContains(t, statements, name, "a name is never text of the statement")
		}
		var values map[string]string
		require.NoError(t, json.Unmarshal([]byte(lines(t, e.dir, "psql-values.log")[0]), &values))
		assert.Equal(t, "chi'no", values["chino"], "each name is a value, whole")
		assert.Equal(t, "kat alog", values["katalog"])
		assert.Equal(t, "key;cloak", values["keycloak"])
	})

	t.Run("R.C7 a Postgres without a claim of its own: its databases, and no volume", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		sizes(t, e, defaultSizes)
		env := libraryEnv(claim)
		delete(env, "POSTGRES_VOLUME")
		out := e.run(env)
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Contains(t, out.report, "databases")
		assert.NotContains(t, out.report, "postgres")
		assert.NotContains(t, strings.Join(lines(t, e.dir, "psql.log"), "\n"), "FROM PROGRAM")
	})

	t.Run("R.C7 a server that may not read its own volume is no failure", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		sizes(t, e, defaultSizes)
		e.write("datadir", datadir)
		e.write("copy-error", "ERROR:  42501: permission denied to COPY to or from an external program\n"+
			"DETAIL:  Only roles with privileges of the \"pg_execute_server_program\" role may COPY to or from an external program.\n"+
			"HINT:  Anyone can COPY to stdout or from stdin. psql's \\copy command also works for anyone.\n"+
			"LOCATION:  DoCopy, copy.c:96\n")
		out := e.run(libraryEnv(claim))
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.NotContains(t, out.report, "failed")
		assert.Equal(t, map[string]any{"refused": "permission denied to COPY to or from an external program"}, out.report["postgres"],
			"psql's error line, which the condition gives")
		assert.Contains(t, out.report, "databases", "the databases' sizes were measured")

		e.write("copy-error", "ERROR:  38000: program \"stat -f -c '%S %b %a %i' '"+datadir+"'\" failed\n"+
			"DETAIL:  child process exited with exit code 127\n")
		out = e.run(libraryEnv(claim))
		assert.Equal(t, 1, out.exit, "any other error stays a failure: it is told by its code, not its words")
		assert.Equal(t, "cannot read the filesystem at "+datadir, out.report["failed"])
		assert.NotContains(t, out.report, "postgres")
	})

	t.Run("R.C7 what a failure leaves: the claim measured, and the reason", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		sizes(t, e, defaultSizes)
		e.write("datadir", datadir)
		e.stat(datadir, "4096 2500 2000 99ff")

		e.write("ready", "2")
		out := e.run(libraryEnv(claim))
		assert.Equal(t, 1, out.exit)
		assert.Equal(t, "the Postgres at postgres does not answer", out.report["failed"])
		assert.EqualValues(t, 4096000, out.report["size"], "what was measured is in the report")
		assert.NotContains(t, out.report, "databases")
		e.write("ready", "0")

		env := libraryEnv(claim)
		env["PGPASSWORD"] = ""
		out = e.run(env)
		assert.Equal(t, "Secret zaentrum-db holds no user and password", out.report["failed"])

		sizes(t, e, map[string]int64{"chino": 1, "katalog": 2, "keycloak": 3})
		out = e.run(libraryEnv(claim))
		assert.Equal(t, "the Postgres has no database portal", out.report["failed"])
		sizes(t, e, defaultSizes)

		e.stat(datadir, "nothing to read")
		out = e.run(libraryEnv(claim))
		assert.Equal(t, "cannot read the filesystem at "+datadir, out.report["failed"])
	})
}

// A report is JSON whatever a name, a version or a reason holds: a quote and
// a backslash are written as JSON writes them.
func TestRC7MeasureScriptReportStaysJSON(t *testing.T) {
	t.Run("R.C7 a reason with a quote and a backslash", func(t *testing.T) {
		e := newMeasureEnv(t)
		missing := filepath.Join(t.TempDir(), `no"such\dir`)
		out := e.run(map[string]string{"MEASURE_DIR": missing})
		assert.Equal(t, missing+" is not a directory this measuring can read", out.report["failed"])

		claim := e.claim("4096 1000 250 1a2b")
		sizes(t, e, map[string]int64{"chino": 1, "katalog": 2, "keycloak": 3})
		env := libraryEnv(claim)
		env["DB_PORTAL"] = `por"t\al`
		delete(env, "POSTGRES_VOLUME")
		out = e.run(env)
		assert.Equal(t, `the Postgres has no database por"t\al`, out.report["failed"])
	})

	t.Run("R.C7 a version with a backslash", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		set(t, claim, "2026-11-15T00-00-05Z", map[string]string{"SHA256SUMS": "x",
			"backup.json": `{"platform":{"uid":"u","version":"v1\q"}}`})
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true"})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Equal(t, `v1\q`, listed(t, out.report, "sets")[0][1])
	})
}

func lines(t *testing.T, dir, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

const (
	mine   = "6b2f0c1e-0000-4000-8000-000000000001"
	others = "6b2f0c1e-0000-4000-8000-000000000002"
)

// record is a set's backup.json, its first field the platform, as the
// backup's Job writes it, and after it a resource and addons whose versions
// are not the platform's.
func record(uid, version string, pretty bool) string {
	r := fmt.Sprintf(`{"platform":{"name":"zaentrum","uid":%q,"version":%q,"operator":"sha-1"},"v":1,`+
		`"createdAt":"2026-11-15T00:00:05Z","resource":{"spec":{"version":"latest","uid":"nobody"}},`+
		`"addons":[{"name":"one","chart":"one","version":"9.9.9"}]}`, uid, version)
	if !pretty {
		return r
	}
	var v any
	_ = json.Unmarshal([]byte(r), &v)
	raw, _ := json.MarshalIndent(v, "", "  ")
	// MarshalIndent sorts no keys of a struct, but a map's: put platform first
	// again, as the backup's Job does.
	s := string(raw)
	i := strings.Index(s, `  "platform"`)
	j := strings.Index(s[i:], "\n  },\n") + i + len("\n  },\n")
	return "{\n" + s[i:j] + strings.TrimPrefix(s[:i], "{\n") + s[j:]
}

func TestRC7MeasureScriptListsTheSets(t *testing.T) {
	platform := fmt.Sprintf(`{"name":"zaentrum","uid":%q,"version":"latest"}`, mine)

	t.Run("R.C7 the sets: newest first, their version and maker from the platform of their backup.json, whole by its checksums", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		set(t, claim, "2026-11-15T00-00-05Z", map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, "v0.5.0", false), "chino.dump": "data"})
		set(t, claim, "2026-11-14T00-00-05Z", map[string]string{"SHA256SUMS": "x", "backup.json": record(others, "v0.4.0", true)})
		set(t, claim, "2026-11-13T00-00-05Z", map[string]string{"chino.dump": "data"})
		set(t, claim, "2026-11-12T00-00-05Z", map[string]string{"SHA256SUMS": "x",
			"backup.json": `{"resource":{"spec":{"version":"latest"}},"platform":{"uid":"` + mine + `","version":"v0.3.0"}}`})
		set(t, claim, ".partial-2026-11-16T00-00-05Z", map[string]string{"globals.sql": "x"})
		set(t, claim, "notes", map[string]string{"a": "x"})
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.EqualValues(t, 4, out.report["total"])
		assert.Equal(t, [][4]any{
			{"2026-11-15T00-00-05Z", "v0.5.0", true, true},
			{"2026-11-14T00-00-05Z", "v0.4.0", true, false},
			{"2026-11-13T00-00-05Z", "", false, ""},
			{"2026-11-12T00-00-05Z", "", true, ""},
		}, listed(t, out.report, "sets"),
			"a version or a uid further on, the resource's or an addon's, is never the platform's; nor is a platform that is not the first field")
		assert.Contains(t, out.stdout, "measure: 4 sets, the newest 4 reported\n")

		out = e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true"})
		for _, s := range listed(t, out.report, "sets") {
			assert.Equal(t, "", s[3], "without the ConfigMap nothing says who made %s", s[0])
		}
	})

	// 35 sets, one an hour, the newest last.
	many := func(t *testing.T, claim string, n int, files map[string]string) []string {
		var names []string
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("2026-10-%02dT%02d-00-05Z", 1+i/24, i%24)
			set(t, claim, name, files)
			names = append([]string{name}, names...)
		}
		return names
	}
	names := func(sets [][4]any) []string {
		var out []string
		for _, s := range sets {
			out = append(out, s[0].(string))
		}
		return out
	}

	t.Run("R.C7 the newest 30, and how many the claim holds", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		all := many(t, claim, 35, map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, "v0.5.0", false)})
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.EqualValues(t, 35, out.report["total"])
		assert.Equal(t, all[:30], names(listed(t, out.report, "sets")))
		assert.NotContains(t, out.report, "set")
		assert.Contains(t, out.stdout, "measure: 35 sets, the newest 30 reported\n")
	})

	t.Run("R.C7 fewer where 30 do not fit the 4096 bytes, however long the version", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		long := strings.Repeat("v", 120)
		all := many(t, claim, 40, map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, long, false)})
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		assert.Equal(t, 0, out.exit, out.stderr)
		sets := listed(t, out.report, "sets")
		assert.EqualValues(t, 40, out.report["total"], "it always says how many the claim holds")
		assert.Less(t, len(sets), 30)
		assert.Greater(t, len(sets), 10)
		assert.Equal(t, all[:len(sets)], names(sets), "the newest first, as many as fit")
		assert.Equal(t, long, sets[0][1])
		assert.Contains(t, out.stdout, fmt.Sprintf("measure: 40 sets, the newest %d reported\n", len(sets)))
		assert.Greater(t, len(out.raw), 3500, "as many as fit: %d bytes", len(out.raw))
	})

	t.Run("R.C7 asked for one set by name, it reports that one as well, however old", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		all := many(t, claim, 35, map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, "v0.5.0", false)})
		oldest := all[len(all)-1]
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform, "SET": oldest})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.Equal(t, all[:30], names(listed(t, out.report, "sets")))
		assert.Equal(t, [][4]any{{oldest, "v0.5.0", true, true}}, listed(t, out.report, "set"))

		out = e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform, "SET": all[0]})
		assert.Equal(t, all[:30], names(listed(t, out.report, "sets")), "one of the newest is listed once")
		assert.NotContains(t, out.report, "set")

		out = e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform, "SET": "2020-01-01T00-00-00Z"})
		assert.Equal(t, 0, out.exit)
		assert.NotContains(t, out.report, "set", "a set the claim does not hold is not reported")
	})

	t.Run("R.C7 a step that stops says where, and what was measured stays in the report", func(t *testing.T) {
		e := newMeasureEnv(t, "find")
		claim := e.claim("4096 1000 250 1a2b")
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		assert.Equal(t, 1, out.exit)
		assert.Regexp(t, `^the measuring stopped at line [0-9]+ of measure-volume\.sh; the Job log says why$`, out.report["failed"])
		assert.EqualValues(t, 4096000, out.report["size"])
	})

	t.Run("R.C7 a backup.json that cannot be read: the set is listed without a version, and the log says which", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		set(t, claim, "2026-11-15T00-00-05Z", map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, "v0.5.0", false)})
		set(t, claim, "2026-11-14T00-00-05Z", map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, "v0.4.0", false)})
		require.NoError(t, os.Chmod(filepath.Join(claim, "2026-11-15T00-00-05Z", "backup.json"), 0))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(claim, "2026-11-15T00-00-05Z", "backup.json"), 0o600) })
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.EqualValues(t, 2, out.report["total"])
		assert.Equal(t, [][4]any{{"2026-11-15T00-00-05Z", "", true, ""}, {"2026-11-14T00-00-05Z", "v0.4.0", true, true}},
			listed(t, out.report, "sets"), "the listing goes on")
		assert.Contains(t, out.stderr, "measure: 2026-11-15T00-00-05Z: its backup.json cannot be read, so it is listed without a version\n")
	})

	t.Run("R.C7 a version of more than 128 characters is no version", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		set(t, claim, "2026-11-15T00-00-05Z", map[string]string{"backup.json": record(mine, strings.Repeat("v", 129), false)})
		set(t, claim, "2026-11-14T00-00-05Z", map[string]string{"backup.json": record(mine, strings.Repeat("v", 128), false)})
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform})
		sets := listed(t, out.report, "sets")
		assert.Equal(t, "", sets[0][1])
		assert.Equal(t, true, sets[0][3], "who made it is still known")
		assert.Equal(t, strings.Repeat("v", 128), sets[1][1])
		assert.Contains(t, out.stderr, "measure: 2026-11-15T00-00-05Z: its backup.json names a version of more than 128 characters, so it is listed without a version\n")
	})

	t.Run("R.C7 the whole report holds 4096 bytes, the set asked for by name included", func(t *testing.T) {
		e := newMeasureEnv(t)
		claim := e.claim("4096 1000 250 1a2b")
		all := many(t, claim, 40, map[string]string{"SHA256SUMS": "x", "backup.json": record(mine, strings.Repeat("v", 128), false)})
		alone := listed(t, e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform}).report, "sets")
		out := e.run(map[string]string{"MEASURE_DIR": claim, "SETS": "true", "PLATFORM": platform, "SET": all[len(all)-1]})
		assert.Equal(t, 0, out.exit, out.stderr)
		assert.LessOrEqual(t, len(out.raw), 4096)
		assert.Equal(t, [][4]any{{all[len(all)-1], strings.Repeat("v", 128), true, true}}, listed(t, out.report, "set"))
		sets := listed(t, out.report, "sets")
		assert.Equal(t, all[:len(sets)], names(sets), "as many of the newest as fit beside it")
		assert.Less(t, len(sets), len(alone), "the set asked for takes its room from the newest")
		assert.EqualValues(t, 40, out.report["total"])
	})
}
