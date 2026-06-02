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
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// catalogMigrations holds the neutral catalog-schema DDL. These are the tables
// the read API (katalog-api) SELECTs and the import/processing path writes —
// the physical tables CAP used to own as com_nalet_katalog_*, renamed here to
// the katalog_* prefix. Each file is an idempotent CREATE TABLE IF NOT EXISTS
// (+ indexes / seed rows), applied in lexical filename order on every boot.
//
//go:embed migrations/catalog/*.sql
var catalogMigrations embed.FS

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

// EnsureSchema creates this service's own tables (manager_config) AND the
// shared catalog tables it is now the owner of.
//
// The catalog tables (katalog_*) used to be generated and owned by CAP. CAP is
// gone, so this service — the sole writer to the catalog — now applies the
// neutral catalog migrations on boot. The read API only SELECTs these tables,
// so creating them here is safe and keeps a fresh database self-bootstrapping.
//
// Order matters: katalog_items must exist before the FK-referencing tables and
// the overall-status rollup view that reads it, so the embedded migrations are
// applied in lexical filename order (001_, 002_, …). Every statement is
// idempotent (CREATE TABLE/INDEX/VIEW IF NOT EXISTS / CREATE OR REPLACE VIEW /
// INSERT … ON CONFLICT), so this is safe to call on every boot. The whole run
// happens before the server starts serving.
//
// Idempotent: safe to call on every boot.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	const managerConfigDDL = `
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
	if _, err := s.Pool.Exec(ctx, managerConfigDDL); err != nil {
		return fmt.Errorf("ensure manager_config: %w", err)
	}

	return s.applyCatalogMigrations(ctx)
}

// applyCatalogMigrations reads the embedded catalog migration files, sorts them
// by filename, and applies each one in its own statement batch. Each file is
// idempotent so this is safe to run on every boot without a tracking table.
func (s *Store) applyCatalogMigrations(ctx context.Context) error {
	entries, err := fs.ReadDir(catalogMigrations, "migrations/catalog")
	if err != nil {
		return fmt.Errorf("read catalog migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	// Lexical order == migration order (001_, 002_, …); FK-referenced tables
	// and the rollup view come after katalog_items.
	sort.Strings(names)

	for _, name := range names {
		body, err := catalogMigrations.ReadFile("migrations/catalog/" + name)
		if err != nil {
			return fmt.Errorf("read catalog migration %s: %w", name, err)
		}
		if _, err := s.Pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply catalog migration %s: %w", name, err)
		}
		slog.Debug("applied catalog migration", "file", name)
	}
	return nil
}

// isNoRows is a small helper so callers don't import pgx just for the sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
