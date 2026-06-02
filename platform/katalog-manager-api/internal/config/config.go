// Package config holds the env-driven runtime configuration for
// katalog-manager-api. Every value has a sane default so the service can
// boot in scaffold mode (no DB, no broker) and still answer /healthz.
package config

import (
	"os"
	"strings"
)

// Config is the env-driven runtime config.
//
// The names mirror the service contract shared with the admin UI, the
// container manifests, and the edge proxy so all four agree on the same
// wire. Secrets (STREAM_SIGNING_KEY) are optional here — the first-run
// setup flow generates one if the operator doesn't supply it.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// KatalogAPIBaseURL is the in-cluster base URL of the read-only catalog
	// API (the sibling service that clients query). The management plane
	// links to it for cross-checks; it never proxies reads through it.
	KatalogAPIBaseURL string
	// KafkaBrokers is the comma-separated bootstrap broker list. Processing
	// work (transcode / package / enrich) is dispatched by emitting events
	// on stube.processing.task.* — never by a synchronous call.
	KafkaBrokers string
	// PgURL is the Postgres connection string for the config + catalog write
	// database. Empty means scaffold mode (no pool).
	PgURL string
	// OIDCIssuer is the discovery URL of the identity provider. Resolved at
	// runtime from the operator's environment / discovery document — never
	// hard-coded to a specific tenant.
	OIDCIssuer string
	// OIDCAudience is the comma-separated audience accept-list checked
	// against the bearer token's aud claim.
	OIDCAudience string
	// StreamSigningKey is the shared HMAC key the stream plane uses to
	// validate player-issued stream tokens. Optional: the first-run setup
	// generates one and persists it if this is empty.
	StreamSigningKey string
}

// Load reads env vars and returns a Config with defaults so the server can
// come up without any env configured.
func Load() Config {
	return Config{
		Addr:              envOr("ADDR", ":8080"),
		KatalogAPIBaseURL: envOr("KATALOG_API_BASE_URL", "http://katalog-api"),
		KafkaBrokers:      envOr("KAFKA_BROKERS", "kafka:9092"),
		PgURL:             os.Getenv("PG_URL"),
		OIDCIssuer:        os.Getenv("OIDC_ISSUER"),
		OIDCAudience:      envOr("OIDC_AUDIENCE", "stube"),
		StreamSigningKey:  os.Getenv("STREAM_SIGNING_KEY"),
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// SplitCSV splits a comma-separated env value into a trimmed, non-empty
// slice. Exported because the events package needs the same parse for the
// broker list.
func SplitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
