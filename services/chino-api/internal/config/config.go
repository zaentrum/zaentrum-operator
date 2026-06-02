package config

import (
	"os"
	"strings"
)

type Config struct {
	Addr         string
	OIDCIssuer   string
	OIDCAudience string
	OIDCEnabled  bool

	// KatalogBaseURL is the in-cluster URL of katalog-api (Go read-only,
	// ADR-011 split, cloud_katalog_ro Postgres role). Owns the metadata
	// surface chino-api consumes: /api/v1/items, /movies, /series,
	// /episodes, /albums, /genres, /items/{id}, /series/{id}/episodes,
	// /items/{id}/segments, /items/{id}/asset.
	KatalogBaseURL string

	// StreamBaseURL is the in-cluster URL of chino-stream, the Go service
	// that handles HLS + trickplay + per-item /play/info. Distinct from
	// KatalogBaseURL since the stube cutover — playback bytes never
	// touch katalog-api.
	StreamBaseURL string

	// ArtworkBaseURL is the in-cluster URL of katalog-manager-api (the
	// CAP Java write surface). Artwork rows live in itemartworkdata
	// which is owned by the writer; katalog-api hasn't grown an artwork
	// endpoint yet. When it does, flip this to the same value as
	// KatalogBaseURL and the env var becomes a no-op.
	ArtworkBaseURL string

	// PgURL is the libpq-form Postgres URL chino-api uses for user-state
	// (playback progress + future watchlist / history). Same database as
	// the katalog service; different table prefix (com_nalet_chino_*).
	// When empty, progress + telemetry endpoints respond gracefully but
	// don't persist — keeps local dev simple.
	PgURL string

	// AnalyzerBaseURL is the in-cluster URL of katalog-analyzer. The
	// admin packaging endpoint forwards POST/GET /api/v1/admin/items/
	// {id}/package here as POST/GET /api/package/{id}. Cluster-internal
	// only; no auth on the analyzer side beyond NetworkPolicy.
	AnalyzerBaseURL string

	// AdminSubjects is the comma-separated allowlist of Keycloak `sub`
	// values that may call POST /api/v1/admin/* endpoints (currently
	// just the packaging trigger). Empty list = nobody can; useful for
	// disabling the admin surface entirely in non-prod. Future: switch
	// to a role-claim check (realm_access.roles contains "admin") so
	// we don't have to redeploy on team changes.
	AdminSubjects []string

	// StreamSigningKey is the base64-encoded HMAC secret used to mint
	// and verify long-lived stream tokens that stand in for the OIDC
	// access token on <video src> URLs. Must be the SAME value in
	// chino-api and katalog-stream (the proxy forwards `?stream=` as-is
	// and katalog-stream re-verifies it). When empty, both services
	// generate ephemeral random keys at boot; URLs minted by one pod
	// won't validate at another, so the feature only works on a
	// single-pod deployment in that case. Production should set this
	// via a shared k8s Secret.
	StreamSigningKey string

	// OIDC client ids advertised by GET /api/config so a neutral self-host
	// client learns which PUBLIC (no-secret) OIDC client to use per
	// platform, then runs discovery against OIDCIssuer. The operator must
	// register these in their IdP with the device-authorization grant +
	// offline_access enabled. Defaults are the documented convention; a
	// deployment overrides them via env when running multiple realms
	// (e.g. a future staging realm).
	OIDCClientIDTV     string
	OIDCClientIDMobile string
	OIDCClientIDWeb    string

	// PublicBaseURL, when set, is the canonical external origin of this
	// chino-api (e.g. https://chino.example.com). GET /api/config reports
	// "<PublicBaseURL>/api" as apiBase; when empty it derives the origin
	// from the request (X-Forwarded-Proto + Host).
	PublicBaseURL string
}

func Load() Config {
	c := Config{
		Addr:               envDefault("ADDR", ":8080"),
		OIDCIssuer:         envDefault("OIDC_ISSUER", ""),
		OIDCAudience:       envDefault("OIDC_AUDIENCE", "chino-web"),
		OIDCEnabled:        envDefault("OIDC_ENABLED", "true") != "false",
		KatalogBaseURL:     envDefault("KATALOG_BASE_URL", "http://katalog-api.stube.svc.cluster.local"),
		StreamBaseURL:      envDefault("STREAM_BASE_URL", "http://chino-stream.stube.svc.cluster.local"),
		ArtworkBaseURL:     envDefault("ARTWORK_BASE_URL", "http://katalog-manager-api.stube.svc.cluster.local"),
		AnalyzerBaseURL:    envDefault("ANALYZER_BASE_URL", "http://katalog-manager-api.stube.svc.cluster.local"),
		AdminSubjects:      splitCSV(envDefault("ADMIN_SUBJECTS", "")),
		PgURL:              envDefault("PG_URL", ""),
		StreamSigningKey:   envDefault("STREAM_SIGNING_KEY", ""),
		OIDCClientIDTV:     envDefault("OIDC_CLIENT_ID_TV", "chino-tv"),
		OIDCClientIDMobile: envDefault("OIDC_CLIENT_ID_MOBILE", "chino-mobile"),
		OIDCClientIDWeb:    envDefault("OIDC_CLIENT_ID_WEB", "chino-web"),
		PublicBaseURL:      envDefault("PUBLIC_BASE_URL", ""),
	}
	return c
}

func envDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
