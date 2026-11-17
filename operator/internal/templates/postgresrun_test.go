package templates

import (
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// These tests run the bundled Postgres's first-start SQL and the two create-db
// init containers, as the chart renders them, on a real Postgres. They need a
// DISPOSABLE server, named by ZAENTRUM_TEST_DATABASE_URL as a superuser's URL
// (postgresql://user[:password]@host[:port]/postgres), and psql and pg_isready
// on the PATH; they skip without the URL. They make databases with names a
// database has only as a value or a quoted name, and drop them after.
const testDatabaseURL = "ZAENTRUM_TEST_DATABASE_URL"

// givenPostgres is the server a run is given: where it is, who signs in, and
// the environment its tools take.
type givenPostgres struct {
	host, port, user string
	env              []string
}

func testPostgres(t *testing.T) *givenPostgres {
	t.Helper()
	raw := os.Getenv(testDatabaseURL)
	if raw == "" {
		t.Skip(testDatabaseURL + " not set: skipping the runs on a Postgres")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err, testDatabaseURL)
	for _, tool := range []string{"psql", "pg_isready"} {
		_, err := exec.LookPath(tool)
		require.NoError(t, err, "%s is not on the PATH", tool)
	}
	password, _ := u.User.Password()
	pg := &givenPostgres{host: u.Hostname(), port: u.Port(), user: u.User.Username()}
	if pg.port == "" {
		pg.port = "5432"
	}
	pg.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "PGHOST=" + pg.host, "PGPORT=" + pg.port,
		"PGUSER=" + pg.user, "PGPASSWORD=" + password, "PGCONNECT_TIMEOUT=10"}
	// The databases the steps connect to first: zaentrum, the image's
	// POSTGRES_DB, and the user's own, which psql takes by default.
	for _, db := range []string{"zaentrum", pg.user} {
		if !pg.holds(t, db) {
			pg.run(t, `CREATE DATABASE :"db";`, "db", db)
			t.Cleanup(func() { pg.drop(t, db) })
		}
	}
	return pg
}

// run runs a statement on the server's maintenance database, the values as
// psql's variables, and returns what it printed.
func (pg *givenPostgres) run(t *testing.T, sql string, values ...string) string {
	t.Helper()
	args := []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-At", "-d", "postgres"}
	for i := 0; i+1 < len(values); i += 2 {
		args = append(args, "-v", values[i]+"="+values[i+1])
	}
	cmd := exec.Command("psql", args...)
	cmd.Env, cmd.Stdin = pg.env, strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
}

func (pg *givenPostgres) holds(t *testing.T, name string) bool {
	t.Helper()
	return pg.run(t, `SELECT 1 FROM pg_database WHERE datname = :'db';`, "db", name) == "1"
}

func (pg *givenPostgres) drop(t *testing.T, name string) {
	t.Helper()
	pg.run(t, `DROP DATABASE IF EXISTS :"db";`, "db", name)
}

// The first start's SQL, as rendered and run as the image runs its init SQL,
// creates the four databases by their names, whole: one of both cases with a
// hyphen, one that starts with a digit and holds a dot, one with a quote, a
// space and a semicolon, and a reserved word.
func TestTheFirstStartCreatesTheDatabasesOnAPostgres(t *testing.T) {
	pg := testPostgres(t)
	names := []string{"Media-Chino", "1kat.alog", "kat'a log;x", "order"}
	for _, name := range names {
		pg.drop(t, name)
		t.Cleanup(func() { pg.drop(t, name) })
	}
	z := base("zaentrum")
	d := &z.Spec.Databases
	d.Chino, d.Katalog, d.Keycloak, d.Portal = names[0], names[1], names[2], names[3]
	file := filepath.Join(t.TempDir(), "10-create-dbs.sql")
	require.NoError(t, os.WriteFile(file, []byte(initSQL(t, renderCR(t, z))), 0o600))
	// As the image's docker_process_sql runs a file of /docker-entrypoint-initdb.d.
	cmd := exec.Command("psql", "-v", "ON_ERROR_STOP=1", "--username", pg.user, "--no-password", "--no-psqlrc",
		"--dbname", "zaentrum", "-f", file)
	cmd.Env = pg.env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	for _, name := range names {
		assert.True(t, pg.holds(t, name), "%q is created by its name, whole", name)
	}
}

// The portal's and Keycloak's create-db init containers, as rendered, create
// their database on a running Postgres where it is missing, by its name whole,
// and leave it as it is where it is there.
func TestTheCreateDBStepsCreateTheirDatabaseOnAPostgres(t *testing.T) {
	pg := testPostgres(t)
	for _, c := range []struct {
		deployment, container, name string
		set                         func(*zaentrumv1alpha1.DatabasesSpec, string)
		bundled                     map[string]int
	}{
		{"portal-api", "create-db", "2Portal.db-x", func(d *zaentrumv1alpha1.DatabasesSpec, n string) { d.Portal = n },
			map[string]int{"-h postgres ": 1, "@postgres:5432/": 1}},
		{"keycloak", "create-keycloak-db", "key cloak's;db", func(d *zaentrumv1alpha1.DatabasesSpec, n string) { d.Keycloak = n },
			map[string]int{"-h postgres ": 3}},
	} {
		t.Run(c.container, func(t *testing.T) {
			pg.drop(t, c.name)
			t.Cleanup(func() { pg.drop(t, c.name) })
			z := base("zaentrum")
			c.set(&z.Spec.Databases, c.name)
			ic := initContainerOf(t, typedDeployment(t, renderCR(t, z), c.deployment).Spec.Template.Spec, c.container)
			// The script runs as rendered but for where the Postgres is.
			script := ic.Args[0]
			for text, n := range c.bundled {
				require.Equal(t, n, strings.Count(script, text), "%q", text)
			}
			script = strings.ReplaceAll(script, "-h postgres ", "-h "+pg.host+" -p "+pg.port+" ")
			script = strings.ReplaceAll(script, "@postgres:5432/", "@"+net.JoinHostPort(pg.host, pg.port)+"/")
			env := append(pg.env[:len(pg.env):len(pg.env)], "DATABASE="+envByName(ic)["DATABASE"].Value)
			for _, said := range []string{"creat", "already exists"} {
				cmd := exec.Command("sh", "-c", script)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				assert.Contains(t, string(out), said)
				assert.True(t, pg.holds(t, c.name), "%q is there, by its name, whole", c.name)
			}
		})
	}
}
