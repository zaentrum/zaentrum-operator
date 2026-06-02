// Package store is the Postgres data-access layer for the management plane.
// It owns two concerns:
//
//   - the service's own config table (first-run setup + live config), and
//   - the WRITE path over the catalog tables (item edits, deletes, and the
//     import scan that registers items the operator already owns on disk).
//
// The read path lives in the separate read-only catalog API; this service is
// the only writer to the catalog item tables.
package store

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the pgx pool.
type Store struct {
	Pool *pgxpool.Pool
}

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrNoPool is returned when handlers run against a Store whose pool was not
// initialised (scaffold mode — PG_URL unset).
var ErrNoPool = errors.New("no pg pool configured")

// New opens a pgx pool against url. An empty url yields a Store with a nil
// pool (scaffold mode); every method guards against that and returns
// ErrNoPool so the server still boots and /healthz answers.
func New(ctx context.Context, url string) (*Store, error) {
	if url == "" {
		slog.Warn("PG_URL not set; running with no DB pool (scaffold mode)")
		return &Store{}, nil
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close releases the underlying pool, if any.
func (s *Store) Close() {
	if s == nil || s.Pool == nil {
		return
	}
	s.Pool.Close()
}

// Ping checks DB connectivity. Used by /api/manage/setup/status to report the
// `database` health check. Returns nil if the pool is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	return s.Pool.Ping(ctx)
}

// EnsureSchema creates the management-plane's own tables if they don't exist.
// Only the config table is owned outright by this service; the catalog item
// tables are shared (the read API reads them, this service writes them) and
// are created by the platform's migration tooling, not here — so this only
// touches `manager_config`.
//
// Idempotent: safe to call on every boot.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	const ddl = `
CREATE TABLE IF NOT EXISTS manager_config (
	-- single-row table; id is pinned to 1 so UPSERT is trivial.
	id              INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
	configured      BOOLEAN     NOT NULL DEFAULT false,
	display_name    TEXT        NOT NULL DEFAULT '',
	oidc_issuer     TEXT        NOT NULL DEFAULT '',
	oidc_client_id  TEXT        NOT NULL DEFAULT '',
	library_path    TEXT        NOT NULL DEFAULT '',
	-- the stream signing key is the one secret stored here; it never leaves
	-- the service over the config GET (that endpoint redacts it).
	stream_signing_key TEXT     NOT NULL DEFAULT '',
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);`
	_, err := s.Pool.Exec(ctx, ddl)
	return err
}

// isNoRows is a small helper so callers don't import pgx just for the sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
