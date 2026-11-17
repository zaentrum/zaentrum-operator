package templates

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// initContainerOf is the init container of that name.
func initContainerOf(t *testing.T, pod corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.InitContainers {
		if c.Name == name {
			return c
		}
	}
	require.Failf(t, "no init container", "%s", name)
	return corev1.Container{}
}

// initSQL is the SQL the bundled Postgres runs at its first start.
func initSQL(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	cm := find(t, objs, "ConfigMap", "postgres-initdb")
	require.NotNil(t, cm)
	sql, _, _ := unstructured.NestedString(cm.Object, "data", "10-create-dbs.sql")
	require.NotEmpty(t, sql)
	return sql
}

// firstStartSQL is the SQL the bundled Postgres runs at its first start for
// the four names, in the order of their keys.
func firstStartSQL(chino, katalog, keycloak, portal string) string {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return "SELECT convert_from(decode('" + b64(chino) + "', 'base64'), 'UTF8') AS chino,\n" +
		"  convert_from(decode('" + b64(katalog) + "', 'base64'), 'UTF8') AS katalog,\n" +
		"  convert_from(decode('" + b64(keycloak) + "', 'base64'), 'UTF8') AS keycloak,\n" +
		"  convert_from(decode('" + b64(portal) + "', 'base64'), 'UTF8') AS portal\n" +
		"\\gset\n" +
		"CREATE DATABASE :\"chino\";\nCREATE DATABASE :\"katalog\";\nCREATE DATABASE :\"keycloak\";\nCREATE DATABASE :\"portal\";\n"
}

// The bundled Postgres's first start creates each database by its name quoted
// as a name: the SQL holds each name in base64, which Postgres reads back
// into the variable psql quotes (:"chino"), so a reserved word, a name with a
// quote, a space and a semicolon, or one of both cases with a hyphen, is
// never text of the SQL. The SQL is held whole here; it runs on a Postgres
// where one is given (postgresrun_test.go).
func TestTheFirstStartCreatesEachDatabaseByItsQuotedName(t *testing.T) {
	for _, katalog := range append([]string{"katalog"}, quotedOnlyNames...) {
		t.Run(katalog, func(t *testing.T) {
			z := base("zaentrum")
			z.Spec.Databases.Katalog = katalog
			sql := initSQL(t, renderCR(t, z))
			assert.Equal(t, firstStartSQL("chino", katalog, "keycloak", "portal"), sql)
			if katalog != "katalog" {
				assert.NotContains(t, sql, katalog, "a name is never text of the SQL")
			}
		})
	}
}

// The init containers that create the portal's and Keycloak's databases on a
// running Postgres look each up by the name the resource gives, as a value
// psql quotes, and create it by that name quoted as a name; the name reaches
// the script in its environment, whole.
func TestTheCreateDBContainersTakeTheNameAsAValueAndAQuotedName(t *testing.T) {
	for _, c := range []struct {
		deployment, container, byDefault string
		set                              func(*zaentrumv1alpha1.DatabasesSpec, string)
	}{
		{"portal-api", "create-db", "portal", func(d *zaentrumv1alpha1.DatabasesSpec, n string) { d.Portal = n }},
		{"keycloak", "create-keycloak-db", "keycloak", func(d *zaentrumv1alpha1.DatabasesSpec, n string) { d.Keycloak = n }},
	} {
		for _, name := range append([]string{c.byDefault}, quotedOnlyNames...) {
			t.Run(c.container+"/"+name, func(t *testing.T) {
				z := base("zaentrum")
				c.set(&z.Spec.Databases, name)
				ic := initContainerOf(t, typedDeployment(t, renderCR(t, z), c.deployment).Spec.Template.Spec, c.container)
				require.Len(t, ic.Args, 1)
				script := ic.Args[0]
				assert.Equal(t, name, envByName(ic)["DATABASE"].Value, "the name, whole, in the script's environment")
				if name != c.byDefault {
					assert.NotContains(t, script, name, "a name is never text of the script")
				}
				env := []string{"PGUSER=zaentrum", "PGPASSWORD=secret", "DATABASE=" + name}

				run := runScript(t, "sh", script, env, map[string][]string{"postgres": {"postgres", "zaentrum"}})
				require.Equal(t, 0, run.exit, run.output())
				namesOnlyAsValues(t, run.calls, []string{name})
				creates := psqlCalls(run.calls, "CREATE DATABASE")
				require.Len(t, creates, 1, "a database the Postgres lacks is created")
				assert.Equal(t, name, creates[0].Values["db"])
				assert.Contains(t, run.servers["postgres"], name)

				run = runScript(t, "sh", script, env, map[string][]string{"postgres": {"postgres", "zaentrum", name}})
				require.Equal(t, 0, run.exit, run.output())
				assert.Empty(t, psqlCalls(run.calls, "CREATE DATABASE"), "one the Postgres holds is left as it is")
			})
		}
	}
}

// externalDatabase is how the scripts that reach the external catalog's
// database connect: by its name, host, port and sslmode from their
// environment.
const externalDatabase = `DB="postgresql://$PGUSER:$PGPASSWORD@$DATABASE_HOSTS/$DATABASE?sslmode=$DATABASE_SSLMODE"`

// scriptLine is the script's line that starts with prefix, its indent left
// out.
func scriptLine(script, prefix string) string {
	for _, line := range strings.Split(script, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// externalScripts are the catalog's schema init container and the demo's
// enqueue container, rendered with external databases: the first as the
// operator renders it, the second under helm, as the demo runs it.
func externalScripts(t *testing.T, name, host string, port int32, sslmode string) (schema, enqueue corev1.Container) {
	t.Helper()
	z := base("zaentrum-beta")
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = host
	z.Spec.Databases.External.Port = port
	z.Spec.Databases.External.SSLMode = sslmode
	z.Spec.Databases.Katalog = name
	schema = initContainerOf(t, typedDeployment(t, renderCR(t, z), "katalog-manager-api").Spec.Template.Spec, "deploy-schema")
	objs := helmRender(t, map[string]interface{}{
		"jobs": map[string]interface{}{"seed": true},
		"databases": map[string]interface{}{"mode": "external", "katalog": name,
			"external": map[string]interface{}{"host": host, "port": port, "sslmode": sslmode}},
	})
	u := find(t, objs, "Job", "enqueue-processing")
	require.NotNil(t, u)
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &job))
	return schema, job.Spec.Template.Spec.Containers[0]
}

// With external databases, the catalog's schema init container and the
// demo's enqueue Job get the catalog's database name, the host, the port and
// the sslmode in their environment, each whole; none is text of their script.
func TestTheExternalDatabaseReachesItsScriptsInTheirEnvironment(t *testing.T) {
	for _, name := range quotedOnlyNames {
		t.Run(name, func(t *testing.T) {
			schema, enqueue := externalScripts(t, name, "db.example.org", 5433, "verify-full")
			for _, c := range []corev1.Container{schema, enqueue} {
				assert.Equal(t, externalDatabase, scriptLine(c.Args[0], "DB="), "%s: no value of the resource is text of the script", c.Name)
				for _, value := range []string{"db.example.org", "5433", "verify-full"} {
					assert.NotContains(t, c.Args[0], value, c.Name)
				}
				env := envByName(c)
				assert.Equal(t, name, env["DATABASE"].Value, c.Name)
				assert.Equal(t, "db.example.org:5433", env["DATABASE_HOSTS"].Value, c.Name)
				assert.Equal(t, "verify-full", env["DATABASE_SSLMODE"].Value, c.Name)
			}
		})
	}
	schema, enqueue := externalScripts(t, "katalog", "db.example.org", 0, "")
	for _, c := range []corev1.Container{schema, enqueue} {
		env := envByName(c)
		assert.Equal(t, "db.example.org:5432", env["DATABASE_HOSTS"].Value, "%s: the port's default", c.Name)
		assert.Equal(t, "require", env["DATABASE_SSLMODE"].Value, "%s: the sslmode's default", c.Name)
	}

	ic := initContainerOf(t, typedDeployment(t, renderCR(t, base("zaentrum")), "katalog-manager-api").Spec.Template.Spec, "deploy-schema")
	for _, key := range []string{"DATABASE", "DATABASE_HOSTS", "DATABASE_SSLMODE"} {
		assert.NotContains(t, envByName(ic), key, "the bundled Postgres's catalog, as before")
	}
}

// standInPath is the script with each of the image's paths in it put where
// the test keeps them, each path found as often as the script has it.
func standInPath(t *testing.T, script string, paths map[string][2]any) string {
	t.Helper()
	for path, to := range paths {
		require.Equal(t, to[1], strings.Count(script, path), "the script runs as rendered but for where %s is", path)
		script = strings.ReplaceAll(script, path, to[0].(string))
	}
	return script
}

// The two external scripts, as rendered, connect to the hosts their
// environment gives, each with its port, whole: one with characters outside
// the rule, an IPv6 address in square brackets, a name, and two hosts, which
// libpq takes as a list.
func TestTheExternalScriptsConnectToTheHostOfTheirEnvironment(t *testing.T) {
	schema, enqueue := externalScripts(t, "katalog_beta", "db.example.org", 5433, "verify-full")
	files := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(files, "sql"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(files, "media"), 0o700))
	for _, f := range []string{"sql/schema-postgres.sql", "sql/schema-extra.sql", "sql/enqueue.sql", "media/a.mkv", "media/b.mkv", "media/c.mkv"} {
		require.NoError(t, os.WriteFile(filepath.Join(files, f), []byte("-- a stand-in\n"), 0o600))
	}
	sql := files + "/sql/"
	scripts := map[string]string{
		"deploy-schema": standInPath(t, schema.Args[0], map[string][2]any{"/sql/": {sql, 2}}),
		"enqueue": standInPath(t, enqueue.Args[0], map[string][2]any{"/sql/": {sql, 1},
			"/var/lib/katalog/media": {files + "/media", 1}}),
	}
	for _, host := range []string{"d\"b$(x)`y`;z&w|v<u>'t:5433", "[::1]:5433", "db.example.org:5433", "db1.example.org:5433,db2.example.org:5433"} {
		for name, script := range scripts {
			t.Run(name+"/"+host, func(t *testing.T) {
				env := []string{"PGUSER=zaentrum", "PGPASSWORD=secret", "DATABASE=katalog_beta", "DATABASE_HOSTS=" + host,
					"DATABASE_SSLMODE=verify-full"}
				// The server the stand-in answers as: a host as written, its port
				// left out, or the list of hosts with their ports as written.
				server := host
				if !strings.Contains(host, ",") {
					server = host[:strings.LastIndex(host, ":")]
				}
				run := runScript(t, "sh", script, env, map[string][]string{server: {"katalog_beta"}})
				require.Equal(t, 0, run.exit, run.output())
				want := "postgresql://zaentrum:secret@" + host + "/katalog_beta?sslmode=verify-full"
				var read []string
				for _, c := range run.calls {
					require.Equal(t, "psql", c.Tool)
					_, _, _, operands := pgArgs(c.Args, "cdfhoUvpF")
					assert.Equal(t, []string{want}, operands, "psql connects to the host as the environment gives it")
					assert.Equal(t, server, c.Server)
					opts, _, _, _ := pgArgs(c.Args, "cdfhoUvpF")
					read = append(read, opts['f']...)
				}
				if name == "deploy-schema" {
					assert.Equal(t, []string{sql + "schema-postgres.sql", sql + "schema-extra.sql"}, read)
					assert.Contains(t, run.stdout, "schema-extra applied.")
				} else {
					assert.Equal(t, []string{sql + "enqueue.sql"}, read)
					assert.Contains(t, run.stdout, "[enqueue] step summary:")
				}
			})
		}
	}
}

// The external database's host is text wherever it goes, Keycloak's
// environment and the two external scripts', whatever YAML would read in it.
func TestTheExternalDatabasesHostIsTextWhereverItGoes(t *testing.T) {
	for _, host := range []string{"on", "null", "1e3", "db.example.org", "[2001:db8::5]"} {
		z := base("zaentrum-beta")
		z.Spec.Databases.Mode = "external"
		z.Spec.Databases.External.Host = host
		dep := typedDeployment(t, renderCR(t, z), "keycloak")
		assert.Equal(t, host, envByName(dep.Spec.Template.Spec.Containers[0])["KC_DB_URL_HOST"].Value)
		schema, enqueue := externalScripts(t, "katalog", host, 5432, "require")
		assert.Equal(t, host+":5432", envByName(schema)["DATABASE_HOSTS"].Value)
		assert.Equal(t, host+":5432", envByName(enqueue)["DATABASE_HOSTS"].Value)
	}
}

// The backup and the restore get the platform's databases a name a line, each
// whole; Keycloak gets its database's name as text, whatever YAML would read
// in it.
func TestTheDatabasesNamesReachTheirJobsWhole(t *testing.T) {
	z := base("zaentrum")
	z.Spec.Databases.Katalog = "kat'a log;x"
	z.Spec.Databases.Keycloak = "on"
	v := NewValues(z)
	v.RestoreDump = restoreDump
	objs, err := Render(v)
	require.NoError(t, err)
	platform, hooks := SplitHooks(objs)
	want := "chino\nkat'a log;x\non\nportal"
	assert.Equal(t, want, envByName(backupCron(t, platform).Spec.JobTemplate.Spec.Template.Spec.Containers[0])["DATABASES"].Value)
	var job batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(RestoreJob(hooks).Object, &job))
	assert.Equal(t, want, envByName(job.Spec.Template.Spec.Containers[0])["DATABASES"].Value)

	for _, name := range []string{"on", "null", "true", "yes", "kat'a log;x"} {
		z := base("zaentrum")
		z.Spec.Databases.Keycloak = name
		dep := typedDeployment(t, renderCR(t, z), "keycloak")
		assert.Equal(t, name, envByName(dep.Spec.Template.Spec.Containers[0])["KC_DB_URL_DATABASE"].Value)
	}
}

// addressEnv is the value of the environment variable of that name in the
// Deployment's containers, and whether one has it.
func addressEnv(t *testing.T, objs []*unstructured.Unstructured, deployment, name string) (string, bool) {
	t.Helper()
	for _, c := range typedDeployment(t, objs, deployment).Spec.Template.Spec.Containers {
		if e, ok := envByName(c)[name]; ok {
			return e.Value, true
		}
	}
	return "", false
}

// The services' addresses of their databases, with the bundled Postgres and
// with external databases: the names, the host, the port and the sslmode,
// each where it belongs, a name with a hyphen, with both cases, or that starts
// with a digit and holds a dot as it is. With two hosts and a port of their
// own, the port follows every host, so each part reaches the same servers on
// it: Keycloak's after every host but the last, which KC_DB_URL_PORT follows;
// the external scripts' list after each.
func TestTheServicesDatabaseAddresses(t *testing.T) {
	const creds = "postgres://$(DB_USER):$(DB_PASSWORD)@"
	external := base("zaentrum-beta")
	external.Spec.Databases.Mode = "external"
	external.Spec.Databases.External.Host = "db.example.org"
	external.Spec.Databases.External.Port = 5433
	external.Spec.Databases.External.SSLMode = "verify-full"
	d := &external.Spec.Databases
	d.Chino, d.Katalog, d.Keycloak, d.Portal = "Media-Chino", "1kat.alog_2", "KC", "portal-x"
	two := external.DeepCopy()
	two.Spec.Databases.External.Host = "db1.example.org,db2.example.org"
	for _, c := range []struct {
		name string
		z    *zaentrumv1alpha1.Zaentrum
		want map[[2]string]string
	}{
		{"bundled", base("zaentrum"), map[[2]string]string{
			{"chino-api", "PG_URL"}:                          creds + "postgres:5432/chino?sslmode=disable",
			{"katalog-api", "KATALOG_API_PG_URL"}:            creds + "postgres:5432/katalog?sslmode=disable",
			{"portal-api", "PG_URL"}:                         creds + "postgres:5432/portal?sslmode=disable",
			{"katalog-manager-api", "SPRING_DATASOURCE_URL"}: "jdbc:postgresql://postgres:5432/katalog",
			{"keycloak", "KC_DB_URL_HOST"}:                   "postgres",
			{"keycloak", "KC_DB_URL_DATABASE"}:               "keycloak",
		}},
		{"external", external, map[[2]string]string{
			{"chino-api", "PG_URL"}:                          creds + "db.example.org:5433/Media-Chino?sslmode=verify-full",
			{"katalog-api", "KATALOG_API_PG_URL"}:            creds + "db.example.org:5433/1kat.alog_2?sslmode=verify-full",
			{"portal-api", "PG_URL"}:                         creds + "db.example.org:5433/portal-x?sslmode=verify-full",
			{"katalog-manager-api", "SPRING_DATASOURCE_URL"}: "jdbc:postgresql://db.example.org:5433/1kat.alog_2?sslmode=verify-full",
			{"keycloak", "KC_DB_URL_HOST"}:                   "db.example.org",
			{"keycloak", "KC_DB_URL_PORT"}:                   "5433",
			{"keycloak", "KC_DB_URL_PROPERTIES"}:             "?sslmode=verify-full",
			{"keycloak", "KC_DB_URL_DATABASE"}:               "KC",
		}},
		{"two hosts", two, map[[2]string]string{
			{"chino-api", "PG_URL"}:                          creds + "db1.example.org:5433,db2.example.org:5433/Media-Chino?sslmode=verify-full",
			{"katalog-api", "KATALOG_API_PG_URL"}:            creds + "db1.example.org:5433,db2.example.org:5433/1kat.alog_2?sslmode=verify-full",
			{"portal-api", "PG_URL"}:                         creds + "db1.example.org:5433,db2.example.org:5433/portal-x?sslmode=verify-full",
			{"katalog-manager-api", "SPRING_DATASOURCE_URL"}: "jdbc:postgresql://db1.example.org:5433,db2.example.org:5433/1kat.alog_2?sslmode=verify-full",
			{"keycloak", "KC_DB_URL_HOST"}:                   "db1.example.org:5433,db2.example.org",
			{"keycloak", "KC_DB_URL_PORT"}:                   "5433",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs := renderCR(t, c.z)
			for at, want := range c.want {
				got, ok := addressEnv(t, objs, at[0], at[1])
				assert.True(t, ok, "%s has %s", at[0], at[1])
				assert.Equal(t, want, got, "%s %s", at[0], at[1])
			}
			if c.name == "bundled" {
				for _, name := range []string{"KC_DB_URL_PORT", "KC_DB_URL_PROPERTIES"} {
					_, ok := addressEnv(t, objs, "keycloak", name)
					assert.False(t, ok, "keycloak: no %s with the bundled Postgres", name)
				}
			}
		})
	}
	schema, enqueue := externalScripts(t, "katalog", "db1.example.org,db2.example.org", 5433, "verify-full")
	for _, c := range []corev1.Container{schema, enqueue} {
		assert.Equal(t, "db1.example.org:5433,db2.example.org:5433", envByName(c)["DATABASE_HOSTS"].Value, "%s: the port after every host", c.Name)
	}

	external.Spec.Databases.External.SSLMode = ""
	got, _ := addressEnv(t, renderCR(t, external), "keycloak", "KC_DB_URL_PROPERTIES")
	assert.Equal(t, "?sslmode=require", got, "the sslmode's default")
}

// A name, a host or an sslmode outside the rules, which reaches the chart
// where the schema's copy did not refuse it, stays one value in the service's
// address: the container's environment keeps its entries, no more.
func TestTheServicesDatabaseAddressesAreOneValueEach(t *testing.T) {
	const broken = "\n        - name: EXTRA\n          value: x"
	z := base("zaentrum-beta")
	z.Spec.Databases.Mode = "external"
	z.Spec.Databases.External.Host = "db.example.org" + broken
	z.Spec.Databases.External.SSLMode = "require" + broken
	z.Spec.Databases.Chino = "chino" + broken
	z.Spec.Databases.Katalog = "katalog" + broken
	z.Spec.Databases.Portal = "portal" + broken
	objs := renderCR(t, z)
	for _, at := range [][2]string{{"chino-api", "PG_URL"}, {"katalog-api", "KATALOG_API_PG_URL"}, {"portal-api", "PG_URL"},
		{"katalog-manager-api", "SPRING_DATASOURCE_URL"}, {"keycloak", "KC_DB_URL_PROPERTIES"}} {
		got, ok := addressEnv(t, objs, at[0], at[1])
		require.True(t, ok, "%s has %s", at[0], at[1])
		assert.Contains(t, got, broken, "%s %s holds the value whole", at[0], at[1])
		_, extra := addressEnv(t, objs, at[0], "EXTRA")
		assert.False(t, extra, "%s: the value is one entry of its environment", at[0])
	}
}
