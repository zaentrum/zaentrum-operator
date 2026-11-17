package templates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chart's scripts that reach the Postgres are run here, whole, as the
// chart ships them, against stand-ins for the Postgres's tools. The stand-ins
// are this test binary itself: run with pgToolsDir set, it is the tool its
// name says, and answers as the Postgres servers of <dir>/servers.json would,
// each holding the databases listed under its host ("postgres", or "local"
// for the server a script starts itself). Each call is logged to
// <dir>/calls.log, a pgCall a line.
//
// A connection URL is read as libpq reads its parts, the host as it is
// written: what is between the user's part and the port, square brackets
// kept, whatever it holds.
//
// The stand-in psql reads its statement from -c, -f or its input. As psql
// does, it puts in a value for :'name' or :"name" only in what it reads from
// -f or its input, and only one -v gave; anything else reaches the server as
// written, which refuses it. A statement that looks a database up takes the
// name only as :'db'; one that creates a database names it only as :"db", and
// its owner as :"owner"; the one that asks who else is in the platform's
// databases takes their names only as :'dbs', a name a line. A connection to a
// database the server does not hold is refused, as a server refuses it.
const pgToolsDir = "ZAENTRUM_TEST_PGTOOLS"

// init makes this binary a stand-in, before any test runs, when pgToolsDir is
// set.
func init() {
	if dir := os.Getenv(pgToolsDir); dir != "" {
		os.Exit(pgTool(dir, filepath.Base(os.Args[0]), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// pgCall is one call of a stand-in: the tool, its arguments, the server and
// the database it connects to, PGDATABASE, and for psql the statement it was
// given and the values -v gave it.
type pgCall struct {
	Tool       string            `json:"tool"`
	Args       []string          `json:"args"`
	Server     string            `json:"server,omitempty"`
	Database   string            `json:"database,omitempty"`
	PGDatabase string            `json:"pgdatabase,omitempty"`
	SQL        string            `json:"sql,omitempty"`
	Values     map[string]string `json:"values,omitempty"`
}

// pgArgs reads a tool's arguments as getopt does: -x VALUE or -xVALUE for the
// letters of withValue, any other letter as a flag, --name[=value], and the
// rest, and all after --, as operands.
func pgArgs(args []string, withValue string) (opts map[byte][]string, flags string, long map[string]string, operands []string) {
	opts, long = map[byte][]string{}, map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return opts, flags, long, append(operands, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			k, v, _ := strings.Cut(a[2:], "=")
			long[k] = v
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				c := a[j]
				if strings.IndexByte(withValue, c) < 0 {
					flags += string(c)
					continue
				}
				v := a[j+1:]
				if v == "" && i+1 < len(args) {
					i++
					v = args[i]
				}
				opts[c] = append(opts[c], v)
				break
			}
		default:
			operands = append(operands, a)
		}
	}
	return opts, flags, long, operands
}

// pgURL reads postgresql://user:password@host:port/database?settings: the
// host as written, its port left out, or the hosts as written, each with its
// port, where there are several; and the database.
func pgURL(s string) (host, database string, ok bool) {
	rest, ok := strings.CutPrefix(s, "postgresql://")
	if !ok {
		if rest, ok = strings.CutPrefix(s, "postgres://"); !ok {
			return "", "", false
		}
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	hostport, path, _ := strings.Cut(rest, "/")
	database, _, _ = strings.Cut(path, "?")
	host = hostport
	if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.Contains(hostport, ",") &&
		strings.Trim(hostport[i+1:], "0123456789") == "" && !strings.HasSuffix(hostport[:i], ":") {
		host = hostport[:i]
	}
	return host, database, true
}

func lastOf(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

func holdsDatabase(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// psqlVariable is what psql puts a value in for: :'name' and :"name".
var psqlVariable = regexp.MustCompile(`:'([A-Za-z_][A-Za-z0-9_]*)'|:"([A-Za-z_][A-Za-z0-9_]*)"`)

// The stand-ins' answers: a dump's table of contents, which lists three
// tables, and a plain dump, which makes them.
const (
	standInTOC = ";\n; Archive created by a stand-in\n;\n" +
		"215; 1259 16385 TABLE public a app\n216; 1259 16390 TABLE public b app\n217; 1259 16395 TABLE public c app\n" +
		"3301; 0 16385 TABLE DATA public a app\n"
	standInPlainDump = "-- a dump\nCREATE TABLE public.a ();\nCREATE TABLE public.b ();\nCREATE TABLE public.c ();\n"
)

func pgTool(dir, tool string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	servers := map[string][]string{}
	if raw, err := os.ReadFile(filepath.Join(dir, "servers.json")); err == nil {
		_ = json.Unmarshal(raw, &servers)
	}
	save := func() {
		raw, _ := json.Marshal(servers)
		_ = os.WriteFile(filepath.Join(dir, "servers.json"), raw, 0o600)
	}
	call := pgCall{Tool: tool, Args: args, PGDatabase: os.Getenv("PGDATABASE")}
	logCall := func() {
		raw, _ := json.Marshal(call)
		if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, string(raw))
			_ = f.Close()
		}
	}
	opts, _, long, operands := pgArgs(args, "cdfhoUvpF")
	// The server: a URL's host, -h, PGHOST, or the one the script started.
	// The database: -d, the operand, a URL's path, PGDATABASE, or the user's.
	connect := func(operand bool) {
		call.Server = "local"
		if h := lastOf(opts['h']); h != "" {
			call.Server = h
		} else if h := os.Getenv("PGHOST"); h != "" {
			call.Server = h
		}
		db := lastOf(opts['d'])
		if db == "" && operand && len(operands) > 0 {
			db = operands[0]
		}
		if server, name, ok := pgURL(db); ok {
			call.Server, db = server, name
		}
		if db == "" {
			db = os.Getenv("PGDATABASE")
		}
		if db == "" {
			if db = lastOf(opts['U']); db == "" {
				db = os.Getenv("PGUSER")
			}
		}
		call.Database = db
	}
	refused := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n", a...)
		return code
	}

	switch tool {
	case "pg_isready":
		return 0
	case "pg_controldata":
		fmt.Fprintln(stdout, "Latest checkpoint location:           0/15D6A18")
		return 0
	case "pg_dumpall":
		connect(false)
		logCall()
		fmt.Fprint(stdout, "CREATE ROLE zaentrum;\nALTER ROLE zaentrum WITH SUPERUSER;\nCREATE ROLE app;\n")
		return 0
	case "pg_dump":
		connect(true)
		logCall()
		if !holdsDatabase(servers[call.Server], call.Database) {
			return refused(1, "pg_dump: error: connection to server failed: FATAL:  database %q does not exist", call.Database)
		}
		if file := lastOf(opts['f']); file != "" {
			if err := os.WriteFile(file, []byte("PGDMP a dump of a stand-in\n"), 0o600); err != nil {
				return refused(1, "pg_dump: %v", err)
			}
			return 0
		}
		fmt.Fprint(stdout, standInPlainDump)
		return 0
	case "pg_restore":
		if len(operands) != 1 {
			return refused(1, "pg_restore: one dump, not %q", operands)
		}
		if _, err := os.Stat(operands[0]); err != nil {
			return refused(1, "pg_restore: %v", err)
		}
		if _, list := long["list"]; list {
			fmt.Fprint(stdout, standInTOC)
			return 0
		}
		connect(false)
		logCall()
		if !holdsDatabase(servers[call.Server], call.Database) {
			return refused(1, "pg_restore: error: connection to server failed: FATAL:  database %q does not exist", call.Database)
		}
		// --create makes the dump's database again, by the name its file has.
		name := strings.TrimSuffix(filepath.Base(operands[0]), ".dump")
		if !holdsDatabase(servers[call.Server], name) {
			servers[call.Server] = append(servers[call.Server], name)
			save()
		}
		return 0
	case "sha256sum":
		_, flags, _, operands := pgArgs(args, "")
		if strings.Contains(flags, "c") {
			raw, err := os.ReadFile(operands[0])
			if err != nil {
				return refused(1, "sha256sum: %v", err)
			}
			failed := false
			for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
				i := strings.Index(line, "  ")
				if i < 0 {
					i = strings.Index(line, " *")
				}
				if i < 0 {
					fmt.Fprintf(stderr, "sha256sum: no checksum in %q\n", line)
					failed = true
					continue
				}
				content, err := os.ReadFile(line[i+2:])
				sum := sha256.Sum256(content)
				if err != nil || hex.EncodeToString(sum[:]) != line[:i] {
					fmt.Fprintf(stdout, "%s: FAILED\n", line[i+2:])
					failed = true
					continue
				}
				fmt.Fprintf(stdout, "%s: OK\n", line[i+2:])
			}
			if failed {
				return 1
			}
			return 0
		}
		for _, f := range operands {
			content, err := os.ReadFile(f)
			if err != nil {
				return refused(1, "sha256sum: %v", err)
			}
			sum := sha256.Sum256(content)
			fmt.Fprintf(stdout, "%s  %s\n", hex.EncodeToString(sum[:]), f)
		}
		return 0
	case "psql":
	default:
		return refused(2, "no stand-in %s", tool)
	}

	connect(true)
	var sql string
	switch {
	case len(opts['c']) > 0:
		sql = strings.Join(opts['c'], "\n")
	case len(opts['f']) > 0:
		raw, err := os.ReadFile(opts['f'][0])
		if err != nil {
			return refused(1, "psql: %v", err)
		}
		sql = string(raw)
	default:
		raw, _ := io.ReadAll(stdin)
		sql = string(raw)
	}
	call.SQL = sql
	for _, v := range opts['v'] {
		k, value, _ := strings.Cut(v, "=")
		if call.Values == nil {
			call.Values = map[string]string{}
		}
		call.Values[k] = value
	}
	logCall()

	// What psql puts a value in for, and what reaches the server as written.
	for _, m := range psqlVariable.FindAllStringSubmatch(sql, -1) {
		if len(opts['c']) > 0 {
			return refused(1, "ERROR:  syntax error at or near \":\" (psql puts no value in a -c statement)")
		}
		if _, ok := call.Values[m[1]+m[2]]; !ok {
			return refused(3, "ERROR:  syntax error at or near \":\" (-v gave no %s)", m[1]+m[2])
		}
	}
	if !holdsDatabase(servers[call.Server], call.Database) {
		return refused(2, "psql: error: connection to server failed: FATAL:  database %q does not exist", call.Database)
	}

	held := servers[call.Server]
	statement := strings.TrimSuffix(strings.Join(strings.Fields(sql), " "), ";")
	switch {
	case strings.Contains(statement, "FROM pg_database WHERE datname"):
		if !strings.HasSuffix(statement, "FROM pg_database WHERE datname = :'db'") {
			return refused(3, "ERROR:  the statement does not take the database's name as :'db'")
		}
		if holdsDatabase(held, call.Values["db"]) {
			switch {
			case strings.HasPrefix(statement, "SELECT 1 "):
				fmt.Fprintln(stdout, "1")
			case strings.HasPrefix(statement, "SELECT pg_get_userbyid(datdba) "):
				fmt.Fprintln(stdout, "app")
			}
		}
	case strings.Contains(statement, "CREATE DATABASE"):
		if statement != `CREATE DATABASE :"db"` && statement != `CREATE DATABASE :"db" OWNER :"owner"` {
			return refused(3, "ERROR:  the statement does not name the database as :\"db\"")
		}
		if holdsDatabase(held, call.Values["db"]) {
			return refused(3, "ERROR:  database %q already exists", call.Values["db"])
		}
		servers[call.Server] = append(held, call.Values["db"])
		save()
	case strings.Contains(statement, "pg_stat_activity"):
		if !strings.Contains(statement, `WHERE datname = ANY (string_to_array(:'dbs', E'\n'))`) {
			return refused(3, "ERROR:  the statement does not take the databases' names as :'dbs'")
		}
		fmt.Fprintln(stdout)
	case statement == "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1":
		sorted := append([]string(nil), held...)
		sort.Strings(sorted)
		for _, n := range sorted {
			fmt.Fprintln(stdout, n)
		}
	case statement == "SELECT rolname FROM pg_roles":
		fmt.Fprintln(stdout, "zaentrum\napp")
	case strings.HasPrefix(statement, "SELECT count(*) FROM pg_class "), statement == "SELECT count(*) FROM pg_stat_user_tables":
		fmt.Fprintln(stdout, "3")
	case statement == "SELECT schemaname||'.'||relname FROM pg_stat_user_tables ORDER BY 1":
		fmt.Fprintln(stdout, "public.a\npublic.b\npublic.c")
	case statement == "SELECT coalesce(sum(n_live_tup), 0) FROM pg_stat_user_tables":
		fmt.Fprintln(stdout, "12")
	case statement == "SELECT pg_database_size(current_database())":
		fmt.Fprintln(stdout, "1048576")
	case statement == "select 1":
		fmt.Fprintln(stdout, "1")
	case statement == "select count(*) from com_nalet_katalog_items where type in ('movie','episode')":
		fmt.Fprintln(stdout, "2")
	case strings.HasPrefix(statement, "select step,status,count(*) from com_nalet_katalog_itemprocessingsteps "):
		fmt.Fprintln(stdout, " step | status | count\n------+--------+-------\n analyze | pending | 2")
	case statement == "", strings.HasPrefix(statement, "CREATE ROLE "), strings.HasPrefix(statement, "ALTER ROLE "),
		strings.HasPrefix(statement, "-- "):
		// The roles, a dump, or a stand-in of a schema: taken.
	default:
		return refused(2, "the stand-in psql does not answer %q", sql)
	}
	return 0
}

// scriptRun is one run of a script against the stand-ins: how it ended, what
// it said, its termination message, the calls of the stand-ins and the
// databases each server holds after.
type scriptRun struct {
	exit           int
	stdout, stderr string
	termlog        string
	calls          []pgCall
	servers        map[string][]string
}

func (r scriptRun) output() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s\ntermination message: %s", r.exit, r.stdout, r.stderr, r.termlog)
}

// runScript runs script with shell, bash or sh, in workdir, with env and the
// stand-ins first on its PATH, against servers.
func runScript(t *testing.T, shell, script string, env []string, servers map[string][]string) scriptRun {
	t.Helper()
	sh, err := exec.LookPath(shell)
	require.NoError(t, err, "the script runs with %s", shell)
	self, err := os.Executable()
	require.NoError(t, err)
	dir, bin := t.TempDir(), t.TempDir()
	for _, tool := range []string{"pg_isready", "pg_controldata", "pg_dumpall", "pg_dump", "pg_restore", "psql", "sha256sum"} {
		require.NoError(t, os.Symlink(self, filepath.Join(bin, tool)))
	}
	raw, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "servers.json"), raw, 0o600))
	termlog := filepath.Join(dir, "termlog")
	// A script that waits for what never comes ends with the test, not after it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
		pgToolsDir + "=" + dir, "TERMINATION_LOG=" + termlog}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		run.exit = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	if raw, err := os.ReadFile(termlog); err == nil {
		run.termlog = string(raw)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "calls.log")); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var c pgCall
			require.NoError(t, json.Unmarshal([]byte(line), &c), line)
			run.calls = append(run.calls, c)
		}
	}
	raw, err = os.ReadFile(filepath.Join(dir, "servers.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &run.servers))
	return run
}

// chartFile is a file of the chart, as the chart ships it.
func chartFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../platform/chart", name))
	require.NoError(t, err)
	return string(raw)
}

// Names Postgres takes for a database only as a value or as a quoted name: a
// reserved word, a name with a quote, a space and a semicolon, one of both
// cases with a hyphen, and one that starts with a digit and holds a dot, whose
// dump file holds the dot too.
var quotedOnlyNames = []string{"order", "kat'a log;x", "Media-Chino", "1chino.beta"}

// platformDatabases are the platform's four databases, the catalog's by the
// name given.
func platformDatabases(katalog string) []string {
	return []string{"chino", katalog, "keycloak", "portal"}
}

// namesOnlyAsValues holds that no statement psql was given holds one of the
// names as text, and that each name reached psql whole, as a value -v gave.
func namesOnlyAsValues(t *testing.T, calls []pgCall, names []string) {
	t.Helper()
	given := map[string]bool{}
	for _, c := range calls {
		if c.Tool != "psql" {
			continue
		}
		for _, name := range names {
			assert.NotContains(t, c.SQL, name, "a name is never text of a statement")
		}
		for k, v := range c.Values {
			given[v] = true
			if k == "dbs" {
				for _, n := range strings.Split(v, "\n") {
					given[n] = true
				}
			}
		}
	}
	for _, name := range names {
		assert.True(t, given[name], "%q reaches psql whole, as a value", name)
	}
}

// connectsByNameAlone holds that no tool was given one of the names to
// connect by, in -d or as an operand, and that each name reached the tools
// whole in PGDATABASE, as the database they connected to.
func connectsByNameAlone(t *testing.T, calls []pgCall, names []string) {
	t.Helper()
	reached := map[string]bool{}
	for _, c := range calls {
		if c.Tool != "psql" && c.Tool != "pg_dump" {
			continue
		}
		opts, _, _, operands := pgArgs(c.Args, "cdfhoUvpF")
		for _, name := range names {
			assert.NotContains(t, opts['d'], name, "%s connects to %q by its name in PGDATABASE alone", c.Tool, name)
			assert.NotContains(t, operands, name, "%s connects to %q by its name in PGDATABASE alone", c.Tool, name)
		}
		if c.PGDatabase != "" && c.Database == c.PGDatabase {
			reached[c.PGDatabase] = true
		}
	}
	for _, name := range names {
		assert.True(t, reached[name], "a tool connects to %q, whole, by PGDATABASE", name)
	}
}

// psqlCalls are the calls of psql whose statement holds text.
func psqlCalls(calls []pgCall, text string) []pgCall {
	var out []pgCall
	for _, c := range calls {
		if c.Tool == "psql" && strings.Contains(c.SQL, text) {
			out = append(out, c)
		}
	}
	return out
}

// runBackup runs the backup script over a claim of the test's, the Postgres
// holding the databases held.
func runBackup(t *testing.T, databases, held []string) (scriptRun, string) {
	t.Helper()
	claim := t.TempDir()
	run := runScript(t, "bash", chartFile(t, "files/postgres-backup.sh"), []string{
		"PGHOST=postgres", "PGUSER=zaentrum", "PGPASSWORD=secret", "DATABASES=" + strings.Join(databases, "\n"),
		"RETENTION=7", "BACKUP_DIR=" + claim, "PG_WAIT_SECONDS=5",
	}, map[string][]string{"postgres": append([]string{"postgres", "zaentrum"}, held...)})
	return run, claim
}

// The backup asks for each database by its name as a value psql quotes,
// never as text of a statement, and dumps it under its name, whole: a
// reserved word, and a name with a quote, a space and a semicolon, as any
// other.
func TestTheBackupTakesEachDatabasesNameAsAValue(t *testing.T) {
	for _, katalog := range append([]string{"katalog"}, quotedOnlyNames...) {
		t.Run(katalog, func(t *testing.T) {
			names := platformDatabases(katalog)
			run, claim := runBackup(t, names, names)
			require.Equal(t, 0, run.exit, run.output())
			var summary struct {
				Dump      string                    `json:"dump"`
				Databases map[string]map[string]int `json:"databases"`
			}
			require.NoError(t, json.Unmarshal([]byte(run.termlog), &summary), run.termlog)
			var dumped []string
			for name, figures := range summary.Databases {
				dumped = append(dumped, name)
				assert.Equal(t, 3, figures["tables"], name)
			}
			assert.ElementsMatch(t, names, dumped, "the summary names each database as it is")
			sums, err := os.ReadFile(filepath.Join(claim, summary.Dump, "SHA256SUMS"))
			require.NoError(t, err)
			var listed []string
			for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
				listed = append(listed, line[66:])
			}
			want := []string{"globals.sql"}
			for _, name := range names {
				want = append(want, name+".dump")
				_, err := os.Stat(filepath.Join(claim, summary.Dump, name+".dump"))
				assert.NoError(t, err, "the dump of %q is in the set", name)
			}
			assert.Equal(t, want, listed, "SHA256SUMS lists each file by its whole name")

			namesOnlyAsValues(t, run.calls, names)
			connectsByNameAlone(t, run.calls, names)
			var asked []string
			for _, c := range psqlCalls(run.calls, "FROM pg_database") {
				asked = append(asked, c.Values["db"])
			}
			assert.Equal(t, names, asked, "each database is asked for by its name, as a value")
		})
	}

	run, _ := runBackup(t, platformDatabases("katalog"), []string{"chino", "katalog", "keycloak"})
	assert.Equal(t, 1, run.exit)
	assert.Equal(t, "the Postgres has no database portal", run.termlog, "a database that is not there is still said")

	run, _ = runBackup(t, []string{"", ""}, nil)
	assert.Equal(t, 1, run.exit)
	assert.Equal(t, "DATABASES names no database", run.termlog, "a list of empty lines names none")
}

// restoreDump is the dump the restore tests restore.
const restoreDump = "2026-10-04T00-00-05Z"

// runRestore runs the restore script over a claim holding a whole dump of the
// databases.
func runRestore(t *testing.T, databases []string) scriptRun {
	t.Helper()
	claim := t.TempDir()
	set := filepath.Join(claim, restoreDump)
	require.NoError(t, os.Mkdir(set, 0o700))
	files := map[string]string{"globals.sql": "CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\n"}
	for _, name := range databases {
		files[name+".dump"] = "PGDMP a dump of a stand-in\n"
	}
	var sums []string
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(set, name), []byte(content), 0o600))
		sum := sha256.Sum256([]byte(content))
		sums = append(sums, hex.EncodeToString(sum[:])+"  "+name)
	}
	sort.Strings(sums)
	require.NoError(t, os.WriteFile(filepath.Join(set, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o600))
	return runScript(t, "bash", chartFile(t, "files/postgres-restore.sh"), []string{
		"PGHOST=postgres", "PGUSER=zaentrum", "PGPASSWORD=secret", "DUMP=" + restoreDump,
		"DATABASES=" + strings.Join(databases, "\n"), "BACKUP_DIR=" + claim,
		"PG_WAIT_SECONDS=5", "CLIENT_WAIT_SECONDS=5",
	}, map[string][]string{"postgres": {"postgres", "zaentrum"}})
}

// The restore asks who else is in the databases with their names as one value
// psql quotes, a name a line, never as text of the statement, and restores
// each database from the dump of its name: a reserved word, and a name with a
// quote, a space and a semicolon, as any other.
func TestTheRestoreTakesEachDatabasesNameAsAValue(t *testing.T) {
	for _, katalog := range append([]string{"katalog"}, quotedOnlyNames...) {
		t.Run(katalog, func(t *testing.T) {
			names := platformDatabases(katalog)
			run := runRestore(t, names)
			require.Equal(t, 0, run.exit, run.output())
			parts := make([]string, len(names))
			for i, name := range names {
				parts[i] = name + " (3 tables)"
			}
			assert.Equal(t, "restored "+strings.Join(parts, ", ")+" from "+restoreDump, run.termlog)

			namesOnlyAsValues(t, run.calls, names)
			connectsByNameAlone(t, run.calls, names)
			sessions := psqlCalls(run.calls, "pg_stat_activity")
			require.Len(t, sessions, 1)
			assert.Equal(t, strings.Join(names, "\n"), sessions[0].Values["dbs"], "the names are one value, a name a line, each whole")
			var restored []string
			for _, c := range run.calls {
				if c.Tool == "pg_restore" {
					restored = append(restored, strings.TrimSuffix(filepath.Base(c.Args[len(c.Args)-1]), ".dump"))
				}
			}
			assert.Equal(t, names, restored, "each database from the dump of its name")
			assert.Subset(t, run.servers["postgres"], names)
		})
	}
}

// entrypointStandIn stands for the image's /usr/local/bin/docker-entrypoint.sh:
// its functions make an empty cluster on the volume, with the maintenance
// database and POSTGRES_DB.
const entrypointStandIn = `docker_setup_env() { :; }
docker_create_db_directories() { mkdir -p "$PGDATA"; }
docker_verify_minimum_env() { :; }
docker_init_database_dir() { echo 16 >"$PGDATA/PG_VERSION"; }
pg_setup_hba_conf() { :; }
docker_temp_server_start() { :; }
docker_setup_db() { :; }
docker_temp_server_stop() { :; }
`

// runMigrate runs the migration script onto an empty volume, the running
// Postgres holding the databases; the image's entrypoint is the stand-in's.
func runMigrate(t *testing.T, databases []string) scriptRun {
	t.Helper()
	script := chartFile(t, "files/postgres-migrate.sh")
	const entrypoint = "/usr/local/bin/docker-entrypoint.sh"
	require.Equal(t, 1, strings.Count(script, entrypoint), "the script runs as shipped but for where the image's entrypoint is")
	standIn := filepath.Join(t.TempDir(), "docker-entrypoint.sh")
	require.NoError(t, os.WriteFile(standIn, []byte(entrypointStandIn), 0o600))
	volume := t.TempDir()
	return runScript(t, "bash", strings.Replace(script, entrypoint, standIn, 1), []string{
		"POSTGRES_USER=zaentrum", "POSTGRES_PASSWORD=secret", "POSTGRES_DB=zaentrum",
		"PGDATA=" + filepath.Join(volume, "pgdata"), "SOURCE_HOST=postgres", "TARGET=postgres-data", "SOURCE_WAIT_SECONDS=5",
	}, map[string][]string{
		"postgres": append([]string{"postgres", "zaentrum"}, databases...),
		"local":    {"postgres", "zaentrum"},
	})
}

// The migration looks each database the Postgres lists up by its name as a
// value psql quotes, and creates it on the volume by its name quoted as a
// name, its owner's as well; never as text of a statement: a reserved word,
// and a name with a quote, a space and a semicolon, are copied as any other.
func TestTheMigrationTakesEachDatabasesNameAsAValueAndAQuotedName(t *testing.T) {
	for _, katalog := range append([]string{"katalog"}, quotedOnlyNames...) {
		t.Run(katalog, func(t *testing.T) {
			names := platformDatabases(katalog)
			run := runMigrate(t, names)
			require.Equal(t, 0, run.exit, run.output())

			all := append([]string{"postgres", "zaentrum"}, names...)
			sort.Strings(all)
			parts := make([]string, len(all))
			for i, name := range all {
				parts[i] = " " + name + " (3 tables, 12 rows)"
			}
			assert.Equal(t, fmt.Sprintf("copied %d databases (%d MiB) onto postgres-data:%s", len(all), len(all), strings.Join(parts, ";")), run.termlog)

			namesOnlyAsValues(t, run.calls, names)
			connectsByNameAlone(t, run.calls, names)
			var created []string
			for _, c := range psqlCalls(run.calls, "CREATE DATABASE") {
				assert.Equal(t, "local", c.Server, "created on the volume")
				assert.Equal(t, `CREATE DATABASE :"db" OWNER :"owner";`, strings.TrimSpace(c.SQL))
				assert.Equal(t, "app", c.Values["owner"], "by its owner on the source")
				created = append(created, c.Values["db"])
			}
			sorted := append([]string(nil), names...)
			sort.Strings(sorted)
			assert.Equal(t, sorted, created, "each database the volume lacks, by its name, whole; postgres and zaentrum are there")
			assert.ElementsMatch(t, all, run.servers["local"], "the volume holds every database the Postgres holds")
		})
	}
}
