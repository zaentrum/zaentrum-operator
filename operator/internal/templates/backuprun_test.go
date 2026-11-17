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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The backup's and the restore's scripts (files/postgres-backup.sh,
// files/postgres-restore.sh) are run here, whole, as the chart ships them, with
// bash, over folders of the test's own, against stand-ins for the Postgres
// tools. The stand-ins are this test binary itself: run with pgDir set, it is
// the tool its name says, as a Postgres with the platform's databases answers:
//
//   - pg_isready: ready;
//   - pg_dumpall --globals-only: the roles, the superuser's among them;
//   - pg_dump -d DB -f FILE: FILE holds "custom dump of DB";
//   - pg_restore --list FILE: three tables, for a FILE pg_dump wrote, else a
//     failure; pg_restore ... FILE: FILE is restored, as <dir>/restored lists;
//   - psql: the answers of the scripts' queries; the roles' SQL from stdin.
//
// sha256sum, sort, find and the rest are the system's.
const pgDir = "ZAENTRUM_TEST_PG"

func init() {
	if dir := os.Getenv(pgDir); dir != "" {
		os.Exit(fakePostgresTool(dir, filepath.Base(os.Args[0]), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// pgTables is the table of contents of every dump: three tables and their
// data.
const pgTables = `;
; Archive created at 2026-11-15 00:00:05 UTC
;
215; 1259 16386 TABLE public accounts zaentrum
216; 1259 16390 TABLE public lists zaentrum
217; 1259 16394 TABLE public progress zaentrum
3301; 0 16386 TABLE DATA public accounts zaentrum
3302; 0 16390 TABLE DATA public lists zaentrum
3303; 0 16394 TABLE DATA public progress zaentrum
`

func fakePostgresTool(dir, tool string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	arg := func(flag string) string {
		for i := range args {
			if args[i] == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	has := func(flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	switch tool {
	case "pg_isready":
		return 0
	case "pg_dumpall":
		fmt.Fprint(stdout, "CREATE ROLE zaentrum;\nALTER ROLE zaentrum WITH SUPERUSER LOGIN;\nCREATE ROLE chino;\nALTER ROLE chino WITH LOGIN;\n")
		return 0
	case "pg_dump":
		// The database: -d, or PGDATABASE, as pg_dump takes it.
		db := arg("-d")
		if db == "" {
			db = os.Getenv("PGDATABASE")
		}
		if err := os.WriteFile(arg("-f"), []byte("custom dump of "+db), 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "pg_restore":
		file := args[len(args)-1]
		raw, err := os.ReadFile(file)
		if err != nil || !strings.HasPrefix(string(raw), "custom dump of ") {
			fmt.Fprintln(stderr, "pg_restore: error: input file does not appear to be a valid archive")
			return 1
		}
		if has("--list") {
			fmt.Fprint(stdout, pgTables)
			return 0
		}
		appendLine(dir, "restored", filepath.Base(file))
		return 0
	case "psql":
		sql := arg("-c")
		if sql == "" {
			// What psql reads from its input: a database looked up by its name
			// as a value, the sessions of the databases by theirs, or roles.
			raw, _ := io.ReadAll(stdin)
			switch in := string(raw); {
			case strings.Contains(in, "FROM pg_database WHERE datname = :'db'"):
				fmt.Fprintln(stdout, "1")
			case strings.Contains(in, "pg_stat_activity"):
			default:
				appendLine(dir, "roles.sql", strings.TrimSpace(in))
			}
			return 0
		}
		switch {
		case strings.Contains(sql, "FROM pg_database WHERE datname"):
			fmt.Fprintln(stdout, "1")
		case strings.Contains(sql, "count(*) FROM pg_class"):
			fmt.Fprintln(stdout, "3")
		case strings.Contains(sql, "pg_stat_activity"):
		case strings.Contains(sql, "FROM pg_roles"):
			fmt.Fprintln(stdout, "zaentrum\nchino")
		default:
			fmt.Fprintf(stderr, "the stand-in psql does not answer %q\n", sql)
			return 2
		}
		return 0
	}
	fmt.Fprintf(stderr, "no stand-in %s\n", tool)
	return 2
}

// pgEnv is the stand-ins' state, the backups' claim and the record.
type pgEnv struct {
	t                  *testing.T
	dir, bin           string
	claim, record      string
	stdout, stderr     string
	termination        string
	databasesSeparated string
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	e := &pgEnv{t: t, dir: t.TempDir(), bin: t.TempDir(), claim: t.TempDir(), record: t.TempDir(),
		databasesSeparated: "chino\nkatalog\nkeycloak\nportal"}
	self, err := os.Executable()
	require.NoError(t, err)
	for _, tool := range []string{"pg_isready", "pg_dumpall", "pg_dump", "pg_restore", "psql"} {
		require.NoError(t, os.Symlink(self, filepath.Join(e.bin, tool)))
	}
	return e
}

// writeRecord is the ConfigMap zaentrum-backup as its volume shows it: a file a
// key.
func (e *pgEnv) writeRecord(parts map[string]string) {
	e.t.Helper()
	for k, v := range parts {
		require.NoError(e.t, os.WriteFile(filepath.Join(e.record, k), []byte(v), 0o600))
	}
}

// run runs a chart's script as its Job does; it returns the exit code.
func (e *pgEnv) run(script string, env map[string]string) int {
	e.t.Helper()
	bash, err := exec.LookPath("bash")
	require.NoError(e.t, err, "the scripts run with bash")
	_, err = exec.LookPath("sha256sum")
	require.NoError(e.t, err, "the scripts check with sha256sum")
	body, err := os.ReadFile(filepath.Join("../../platform/chart/files", script))
	require.NoError(e.t, err)
	termlog := filepath.Join(e.dir, "termlog")
	_ = os.Remove(termlog)
	cmd := exec.Command(bash, "-c", string(body))
	cmd.Env = []string{"PATH=" + e.bin + ":" + os.Getenv("PATH"), "HOME=" + e.dir, "TMPDIR=" + e.dir,
		pgDir + "=" + e.dir, "TERMINATION_LOG=" + termlog, "PGHOST=postgres", "PGUSER=zaentrum", "PGPASSWORD=secret",
		"DATABASES=" + e.databasesSeparated, "BACKUP_DIR=" + e.claim, "PG_WAIT_SECONDS=5", "CLIENT_WAIT_SECONDS=5"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	e.stdout, e.stderr = stdout.String(), stderr.String()
	raw, _ := os.ReadFile(termlog)
	e.termination = string(raw)
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	require.NoError(e.t, err)
	return 0
}

// backup runs a backup and returns the set it made.
func (e *pgEnv) backup() string {
	e.t.Helper()
	require.Equal(e.t, 0, e.run("postgres-backup.sh", map[string]string{"RETENTION": "7", "RECORD_DIR": e.record}),
		"the backup ends well: %s", e.stderr)
	var summary struct {
		Dump string `json:"dump"`
	}
	require.NoError(e.t, json.Unmarshal([]byte(e.termination), &summary), e.termination)
	require.NotEmpty(e.t, summary.Dump)
	return summary.Dump
}

// files are the names in a set's directory, sorted.
func (e *pgEnv) files(set string) []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.claim, set))
	require.NoError(e.t, err)
	var out []string
	for _, f := range entries {
		out = append(out, f.Name())
	}
	sort.Strings(out)
	return out
}

// listed are the names a set's SHA256SUMS lists, sorted.
func (e *pgEnv) listed(set string) []string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.claim, set, "SHA256SUMS"))
	require.NoError(e.t, err)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		require.Len(e.t, fields, 2, line)
		out = append(out, strings.TrimPrefix(fields[1], "*"))
	}
	sort.Strings(out)
	return out
}

var aRecord = map[string]string{
	"platform": `{"name":"zaentrum","operator":"v0.5.1","uid":"6b2f0c1e-0000-4000-8000-000000000001","version":"v0.5.0"}`,
	"resource": `{"hostname":"media.example.org","version":"v0.5.0"}`,
	"secrets":  `["katalog-tmdb","zaentrum-tls"]`,
	"addons":   `[{"chart":"oci://registry.example.org/charts/tv-guide","name":"tv-guide","version":"1.2.0"}]`,
}

func TestRC10TheBackupWritesWhatMadeTheSet(t *testing.T) {
	t.Run("R.C10 backup.json from the ConfigMap, platform first, with when the run began and the databases, in SHA256SUMS", func(t *testing.T) {
		e := newPgEnv(t)
		e.writeRecord(aRecord)
		set := e.backup()
		files := []string{"SHA256SUMS", "backup.json", "chino.dump", "globals.sql", "katalog.dump", "keycloak.dump", "portal.dump"}
		assert.Equal(t, files, e.files(set))
		assert.Equal(t, files[1:], e.listed(set), "SHA256SUMS lists backup.json with the rest")
		check := exec.Command("sha256sum", "-c", "SHA256SUMS")
		check.Dir = filepath.Join(e.claim, set)
		out, err := check.CombinedOutput()
		require.NoError(t, err, "every checksum matches: %s", out)

		raw, err := os.ReadFile(filepath.Join(e.claim, set, "backup.json"))
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(raw), `{"platform":{`), "platform is the file's first field: %s", raw)
		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &got), "backup.json is JSON: %s", raw)
		assert.JSONEq(t, aRecord["platform"], string(got["platform"]))
		assert.JSONEq(t, `1`, string(got["v"]))
		created := set[:13] + ":" + set[14:16] + ":" + set[17:19] + "Z"
		assert.JSONEq(t, fmt.Sprintf("%q", created), string(got["createdAt"]), "when the run began, the set's name")
		dump := len("custom dump of chino")
		assert.JSONEq(t, fmt.Sprintf(`{"chino":{"tables":3,"bytes":%d},"katalog":{"tables":3,"bytes":%d},"keycloak":{"tables":3,"bytes":%d},"portal":{"tables":3,"bytes":%d}}`,
			dump, len("custom dump of katalog"), len("custom dump of keycloak"), len("custom dump of portal")), string(got["databases"]))
		assert.JSONEq(t, aRecord["resource"], string(got["resource"]))
		assert.JSONEq(t, aRecord["secrets"], string(got["secrets"]))
		assert.JSONEq(t, aRecord["addons"], string(got["addons"]))
		assert.Len(t, got, 7, "nothing else")
		assert.NotContains(t, e.stdout, "made without backup.json")

		t.Run("R.C10 the backups' measuring reads the version and the maker from it", func(t *testing.T) {
			m := newMeasureEnv(t)
			m.stat(e.claim, "4096 1310720 1048576 2fa4")
			rep := m.run(map[string]string{"MEASURE_DIR": e.claim, "SETS": "true", "PLATFORM": aRecord["platform"]}).report
			assert.Equal(t, [][4]any{{set, "v0.5.0", true, true}}, listed(t, rep, "sets"))
		})
	})

	t.Run("R.C10 addons not known are written as null, and the set keeps its backup.json", func(t *testing.T) {
		e := newPgEnv(t)
		unknown := map[string]string{}
		for k, v := range aRecord {
			unknown[k] = v
		}
		unknown["addons"] = "null"
		e.writeRecord(unknown)
		set := e.backup()
		raw, err := os.ReadFile(filepath.Join(e.claim, set, "backup.json"))
		require.NoError(t, err, "made with backup.json")
		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &got), "backup.json is JSON: %s", raw)
		assert.JSONEq(t, `null`, string(got["addons"]))
	})

	t.Run("R.C10 a URL reaches the set without its user, its password and its query", func(t *testing.T) {
		z := measured()
		z.Spec.Identity.Issuer = "https://someone:pa55@id.example.org/realms/home?prompt=login"
		v := NewValues(z)
		v.Record = BackupRecord{Operator: "v0.5.1", Addons: []BackupAddon{
			{Name: "tv-guide", Chart: "oci://someone:pa55@registry.example.org/charts/tv-guide?ticket=abc", Version: "1.2.0"}}}
		objs, err := Render(v)
		require.NoError(t, err)
		platform, _ := SplitHooks(objs)
		cm := find(t, platform, "ConfigMap", BackupRecordName)
		require.NotNil(t, cm)
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		e := newPgEnv(t)
		e.writeRecord(data)
		set := e.backup()
		raw, err := os.ReadFile(filepath.Join(e.claim, set, "backup.json"))
		require.NoError(t, err)
		for _, kept := range []string{"pa55", "someone", "prompt=", "ticket="} {
			assert.NotContains(t, string(raw), kept)
		}
		var got struct {
			Resource struct {
				Identity struct{ Issuer string } `json:"identity"`
			} `json:"resource"`
			Addons []BackupAddon `json:"addons"`
		}
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, "https://id.example.org/realms/home", got.Resource.Identity.Issuer)
		assert.Equal(t, []BackupAddon{{Name: "tv-guide", Chart: "oci://registry.example.org/charts/tv-guide", Version: "1.2.0"}}, got.Addons)
	})

	for name, record := range map[string]map[string]string{
		"R.C10 without the ConfigMap the set is made without backup.json, and the log says so": nil,
		"R.C10 a ConfigMap that holds the platform alone is no record":                         {"platform": aRecord["platform"]},
		"R.C10 a part that is no JSON object or list is no record": {"platform": aRecord["platform"], "resource": "the spec",
			"secrets": aRecord["secrets"], "addons": aRecord["addons"]},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPgEnv(t)
			e.writeRecord(record)
			set := e.backup()
			files := []string{"SHA256SUMS", "chino.dump", "globals.sql", "katalog.dump", "keycloak.dump", "portal.dump"}
			assert.Equal(t, files, e.files(set))
			assert.Equal(t, files[1:], e.listed(set))
			assert.Contains(t, e.stdout, "postgres-backup: the ConfigMap zaentrum-backup holds no record of the platform: the set is made without backup.json\n")
		})
	}
}

func TestRC10TheRestoreTakesASetWithBackupJSONOrWithout(t *testing.T) {
	restore := func(e *pgEnv, set string) int {
		e.t.Helper()
		return e.run("postgres-restore.sh", map[string]string{"DUMP": set})
	}
	restored := func(e *pgEnv) []string {
		raw, err := os.ReadFile(filepath.Join(e.dir, "restored"))
		if err != nil {
			return nil
		}
		return strings.Fields(string(raw))
	}

	t.Run("R.C10 a set with backup.json is whole, and restores", func(t *testing.T) {
		e := newPgEnv(t)
		e.writeRecord(aRecord)
		set := e.backup()
		require.Equal(t, 0, restore(e, set), e.stderr)
		assert.Equal(t, "restored chino (3 tables), katalog (3 tables), keycloak (3 tables), portal (3 tables) from "+set, e.termination)
		assert.Equal(t, []string{"chino.dump", "katalog.dump", "keycloak.dump", "portal.dump"}, restored(e))
	})

	t.Run("R.C10 a set made before sets held backup.json restores as before", func(t *testing.T) {
		e := newPgEnv(t)
		set := e.backup()
		require.Equal(t, 0, restore(e, set), e.stderr)
		assert.Len(t, restored(e), 4)
	})

	for name, c := range map[string]struct {
		change func(t *testing.T, dir string)
		says   string
	}{
		"R.C10 backup.json beside the set that its SHA256SUMS does not list": {func(t *testing.T, dir string) {
			sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
			require.NoError(t, err)
			var kept []string
			for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
				if !strings.HasSuffix(line, " backup.json") {
					kept = append(kept, line)
				}
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(kept, "\n")+"\n"), 0o600))
		}, "'s SHA256SUMS lists [chino.dump globals.sql katalog.dump keycloak.dump portal.dump], not the files of the databases " +
			"[backup.json chino.dump globals.sql katalog.dump keycloak.dump portal.dump]; nothing was changed"},
		"R.C10 backup.json that its SHA256SUMS lists and the set lacks": {func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "backup.json")))
		}, " holds [chino.dump globals.sql katalog.dump keycloak.dump portal.dump], not the files its SHA256SUMS lists; nothing was changed"},
		"R.C10 backup.json changed after its checksum": {func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "backup.json"), []byte(`{"platform":{"version":"v9.9.9"}}`), 0o600))
		}, "backup.json: FAILED"},
		"R.C10 any other file, beside backup.json": {func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600))
		}, " holds [backup.json chino.dump globals.sql katalog.dump keycloak.dump notes.txt portal.dump], not the files its SHA256SUMS lists; nothing was changed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPgEnv(t)
			e.writeRecord(aRecord)
			set := e.backup()
			c.change(t, filepath.Join(e.claim, set))
			assert.Equal(t, 3, restore(e, set), "refused: %s", e.stderr)
			assert.True(t, strings.HasPrefix(e.termination, "refused: "+set), e.termination)
			assert.Contains(t, e.termination, c.says)
			assert.True(t, strings.HasSuffix(e.termination, "; nothing was changed"), e.termination)
			assert.Empty(t, restored(e), "nothing was changed")
		})
	}
}
