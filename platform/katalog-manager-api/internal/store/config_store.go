package store

import (
	"context"
	"fmt"
)

// SetupInput is the first-run payload persisted by Save. StreamSigningKey may
// be empty on input — the caller generates one before calling Save when it is.
type SetupInput struct {
	DisplayName      string
	OIDCIssuer       string
	OIDCClientID     string
	LibraryPath      string
	StreamSigningKey string
}

// LoadConfig returns the persisted non-secret config. When the singleton row
// does not exist yet (fresh install, before first-run setup) it returns a
// zero Config with Configured=false and no error — "not configured" is a
// normal state, not a failure.
func (s *Store) LoadConfig(ctx context.Context) (Config, error) {
	if s == nil || s.Pool == nil {
		return Config{}, ErrNoPool
	}
	var c Config
	err := s.Pool.QueryRow(ctx, `
		SELECT configured, display_name, oidc_issuer, oidc_client_id, library_path
		FROM manager_config WHERE id = 1`).
		Scan(&c.Configured, &c.DisplayName, &c.OIDCIssuer, &c.OIDCClientID, &c.LibraryPath)
	if isNoRows(err) {
		return Config{Configured: false}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return c, nil
}

// IsConfigured reports whether first-run setup has completed.
func (s *Store) IsConfigured(ctx context.Context) (bool, error) {
	c, err := s.LoadConfig(ctx)
	if err != nil {
		return false, err
	}
	return c.Configured, nil
}

// StreamSigningKey returns the persisted signing key (secret). Empty string
// when none has been set. Kept separate from LoadConfig so the secret never
// rides along on the non-secret config path by accident.
func (s *Store) StreamSigningKey(ctx context.Context) (string, error) {
	if s == nil || s.Pool == nil {
		return "", ErrNoPool
	}
	var key string
	err := s.Pool.QueryRow(ctx,
		`SELECT stream_signing_key FROM manager_config WHERE id = 1`).Scan(&key)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load stream key: %w", err)
	}
	return key, nil
}

// Save persists the first-run config and marks the install configured. It
// UPSERTs the singleton row so repeated setup calls are idempotent — the last
// write wins. The signing key is only overwritten when a non-empty value is
// supplied, so re-running setup without a key keeps the existing one.
func (s *Store) Save(ctx context.Context, in SetupInput) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO manager_config
			(id, configured, display_name, oidc_issuer, oidc_client_id, library_path, stream_signing_key, updated_at)
		VALUES (1, true, $1, $2, $3, $4, $5, now())
		ON CONFLICT (id) DO UPDATE SET
			configured         = true,
			display_name       = EXCLUDED.display_name,
			oidc_issuer        = EXCLUDED.oidc_issuer,
			oidc_client_id     = EXCLUDED.oidc_client_id,
			library_path       = EXCLUDED.library_path,
			stream_signing_key = CASE
				WHEN EXCLUDED.stream_signing_key <> '' THEN EXCLUDED.stream_signing_key
				ELSE manager_config.stream_signing_key
			END,
			updated_at         = now()`,
		in.DisplayName, in.OIDCIssuer, in.OIDCClientID, in.LibraryPath, in.StreamSigningKey)
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// UpdateConfig applies a non-secret config update. It does not flip the
// configured flag (you can only configure via Save) and never touches the
// signing key. Used by PUT /api/manage/config.
func (s *Store) UpdateConfig(ctx context.Context, c Config) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE manager_config SET
			display_name   = $1,
			oidc_issuer    = $2,
			oidc_client_id = $3,
			library_path   = $4,
			updated_at     = now()
		WHERE id = 1`,
		c.DisplayName, c.OIDCIssuer, c.OIDCClientID, c.LibraryPath)
	if err != nil {
		return fmt.Errorf("update config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// No row yet — caller must run first-run setup before PUT config.
		return ErrNotFound
	}
	return nil
}
