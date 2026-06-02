package store

import (
	"context"
	"fmt"
)

// EnsureJobsSchema creates the lightweight job-history table this service owns
// for surfacing recent scan/processing dispatches in the admin UI. The
// authoritative per-step pipeline state still lives on the catalog item rows;
// this table is a convenience log of work this service has enqueued.
//
// Idempotent.
func (s *Store) EnsureJobsSchema(ctx context.Context) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	const ddl = `
CREATE TABLE IF NOT EXISTS manager_jobs (
	id          TEXT PRIMARY KEY,
	kind        TEXT        NOT NULL,           -- scan | transcode | package | enrich
	status      TEXT        NOT NULL DEFAULT 'queued',
	item_id     TEXT,
	detail      TEXT        NOT NULL DEFAULT '',
	started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_manager_jobs_started ON manager_jobs (started_at DESC);`
	_, err := s.Pool.Exec(ctx, ddl)
	return err
}

// RecordJob inserts a job row in the 'queued' state and returns its id. Called
// right after an event is published so the admin UI can show the dispatch even
// before any worker reports back.
func (s *Store) RecordJob(ctx context.Context, id, kind, itemID, detail string) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO manager_jobs (id, kind, status, item_id, detail)
		VALUES ($1, $2, 'queued', NULLIF($3, ''), $4)`,
		id, kind, itemID, detail)
	if err != nil {
		return fmt.Errorf("record job: %w", err)
	}
	return nil
}

// ListJobs returns recent jobs, newest first. limit is clamped to [1, 200].
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if s == nil || s.Pool == nil {
		return nil, ErrNoPool
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, kind, status, COALESCE(item_id, ''),
		       COALESCE(to_char(started_at,  'YYYY-MM-DD"T"HH24:MI:SSOF'), ''),
		       COALESCE(to_char(finished_at, 'YYYY-MM-DD"T"HH24:MI:SSOF'), ''),
		       detail
		FROM manager_jobs
		ORDER BY started_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0, limit)
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Status, &j.ItemID,
			&j.StartedAt, &j.FinishedAt, &j.Detail); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
