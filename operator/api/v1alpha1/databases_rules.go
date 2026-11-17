package v1alpha1

// The rules of spec.databases. The schema holds them as plain keywords, a
// pattern, a maximum length and an enum, which the cluster checks at a write
// and refuses in its own words; a write of the status checks the status alone.
// The operator checks a stored resource with the same rules (refusal.go in the
// controller), for a value stored before the schema had them. The markers on
// DatabasesSpec and DatabaseExternalSpec write the same rules into the schema;
// a test holds the markers, the schema and these alike.
//
// What the rules do not keep out: postgres, template0 and template1 are names
// a platform database may have; and square brackets around anything of hex
// digits, colons and dots pass as a host.
const (
	// DatabaseNamePattern is what a database's name in spec.databases may be:
	// at most 63 ASCII letters, digits, underscores, hyphens and dots,
	// starting with a letter, a digit or an underscore; or empty, for the
	// default. A name holds no slash, so a dump of it is a file of its set;
	// no character a URL reads otherwise; and no dollar sign, which the
	// kubelet would read in a container's environment.
	DatabaseNamePattern   = `^([A-Za-z0-9_][A-Za-z0-9_.-]{0,62})?$`
	DatabaseNameMaxLength = 63

	// databaseHost is one host: a host name of labels of ASCII letters,
	// digits, underscores and hyphens, each 1 to 63 characters, neither
	// starting nor ending with a hyphen, dot-separated, with a trailing dot or
	// without (an IPv4 address is one); or an IPv6 address in square brackets,
	// without a zone, the form a URL takes it in.
	databaseHost = `([[][0-9A-Fa-f:.]+[]]|[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?([.][A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?)*[.]?)`

	// DatabaseHostPattern is the external database's host as the schema holds
	// it: one host or several, separated by commas; or empty, where the
	// databases are not external. A pattern cannot count a host's
	// DatabaseHostMaxLength characters: a longer host passes the schema, and
	// the operator's check, which counts them, refuses it.
	DatabaseHostPattern = `^(|` + databaseHost + `(,` + databaseHost + `)*)$`

	// DatabaseOneHostPattern is one host of the list, as the operator's check
	// holds each, with at most DatabaseHostMaxLength characters, a trailing
	// dot not counted.
	DatabaseOneHostPattern = `^` + databaseHost + `$`
	DatabaseHostMaxLength  = 253
)

// DatabaseSSLModes are the external database's sslmode values: Postgres's
// six, and the empty one, which the platform reads as require.
var DatabaseSSLModes = []string{"", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"}
