package controller

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaRule is a string field's pattern and maximum length as the schema
// holds them, and whether a value meets both, as the API server checks them
// at a write: the pattern is RE2, as Go's regexp is, and the length counts
// characters.
type schemaRule struct {
	pattern   string
	maxLength int
}

func (r schemaRule) takes(value string) bool {
	if r.maxLength > 0 && utf8.RuneCountInString(value) > r.maxLength {
		return false
	}
	return regexp.MustCompile(r.pattern).MatchString(value)
}

// fieldRule is the rule of the field at the path below spec.
func fieldRule(t *testing.T, path ...string) schemaRule {
	t.Helper()
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	require.Len(t, versions, 1)
	field := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec")
	for _, p := range path {
		field = dig(field, "properties", p)
	}
	require.NotNil(t, field, "%v", path)
	assert.Nil(t, dig(field, "x-kubernetes-validations"), "%v: a plain keyword, which a status write does not check", path)
	pattern, _ := dig(field, "pattern").(string)
	require.NotEmpty(t, pattern, "%v", path)
	max, _ := dig(field, "maxLength").(float64)
	return schemaRule{pattern: pattern, maxLength: int(max)}
}

// A database's name in the resource is at most 63 ASCII letters, digits,
// underscores, hyphens and dots, starting with a letter, a digit or an
// underscore, what Postgres keeps of a name; the empty value stays, for the
// default. The schema holds it as a pattern and a maximum length, which the
// cluster checks at a write, in its own words.
func TestADatabasesNameInTheResource(t *testing.T) {
	for _, key := range []string{"chino", "katalog", "keycloak", "portal"} {
		rule := fieldRule(t, "databases", key)
		assert.Equal(t, 63, rule.maxLength, key)
		for _, name := range []string{"", key, "apps_2", "_x", "order", "1chino", "chino.beta", "media-chino", "Chino", "Media-Chino",
			strings.Repeat("a", 63)} {
			assert.True(t, rule.takes(name), "%s: %q is a name a database can have", key, name)
		}
		for _, name := range []string{"chino$prod", "zürich", "-db", ".db", "chi'no", "kat alog", "key;cloak", "chi/no", "chi\x01no",
			strings.Repeat("a", 64), "chino\nx"} {
			assert.False(t, rule.takes(name), "%s: %q is refused", key, name)
		}
	}
}

// hostsOf are the hosts the tests try, shared with the operator's check's
// test: what the schema takes, and what it refuses.
var (
	hostLabel  = func(c string, n int) string { return strings.Repeat(c, n) }
	host253    = hostLabel("a", 63) + "." + hostLabel("b", 63) + "." + hostLabel("c", 63) + "." + hostLabel("d", 61)
	hostsTaken = []string{"", "db", "db-1.example.org", "DB.Example.ORG", "my_db", "10.0.0.5", "[::1]", "[2001:db8::5]",
		"db.example.org.", host253, host253 + ".", "db1.example.org,db2.example.org", "[::1],db.example.org"}
	hostsRefused = []string{"db.example.org:5433", "::1", "2001:db8::5", "db example", "db;x", "db$(x)", `"db"`, "db/x", "u@db", "-db",
		"db-", "a..b", ".db", "db\nx", hostLabel("e", 64) + ".org", "h1,,h2", "db.example.org,", ",db", "[fe80::1%eth0]",
		"zürich.example.org"}
)

// The external database's host in the resource is one host or several,
// separated by commas, each a host name or an IP address in ASCII (an IPv4
// address is one), an IPv6 address in square brackets; or empty where the
// databases are not external. A port is no part of it: the platform takes the
// port from databases.external.port. The schema holds this as a plain
// pattern, which bounds each label to 63 characters but cannot count a host's
// 253: a longer host passes the schema, and the operator's check refuses it.
func TestTheExternalDatabasesHostInTheResource(t *testing.T) {
	rule := fieldRule(t, "databases", "external", "host")
	assert.Zero(t, rule.maxLength, "no maxLength: a list of hosts has none")
	assert.NotContains(t, rule.pattern, `\`, "the pattern holds no backslash")
	for _, h := range hostsTaken {
		assert.True(t, rule.takes(h), "%q is a host", h)
	}
	for _, h := range hostsRefused {
		assert.False(t, rule.takes(h), "%q is refused", h)
	}
	assert.True(t, rule.takes(host253+"d"), "254 characters pass the schema; the operator's check refuses them")
}

// The external database's sslmode in the resource is one of Postgres's six,
// or empty, which the platform reads as its default, require; the API server
// refuses another in its own words for a value it does not list.
func TestTheExternalDatabasesSSLModeInTheResource(t *testing.T) {
	versions, _ := dig(zaentrumCRDSpec(t, canonicalCRD), "versions").([]any)
	require.Len(t, versions, 1)
	sslmode := dig(versions[0], "schema", "openAPIV3Schema", "properties", "spec", "properties", "databases", "properties",
		"external", "properties", "sslmode")
	assert.Equal(t, []any{"", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"}, dig(sslmode, "enum"))
	assert.Equal(t, "require", dig(sslmode, "default"))
}
