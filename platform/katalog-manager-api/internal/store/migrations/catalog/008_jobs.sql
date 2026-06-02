-- catalog schema 008: scan + enrichment job history and the enrichment
-- status code-list.
--
-- Renamed from CAP physical tables:
--   com_nalet_katalog_scanjobs              -> katalog_scanjobs
--   com_nalet_katalog_enrichmentjobs        -> katalog_enrichmentjobs
--   com_nalet_katalog_enrichmentstatuscodes -> katalog_enrichmentstatuscodes
--
-- scanjobs / enrichmentjobs each record one invocation of the corresponding
-- processing pass over files the operator already owns (status fields mirror
-- each other). enrichmentstatuscodes is the small static lookup that backs the
-- enrichment-status dropdown; it is read-only at runtime and seeded here.

CREATE TABLE IF NOT EXISTS katalog_scanjobs (
  id            VARCHAR(36) NOT NULL,
  source        VARCHAR(20) NOT NULL,
  status        VARCHAR(20) NOT NULL DEFAULT 'queued',
  startedat     TIMESTAMP,
  finishedat    TIMESTAMP,
  errormessage  TEXT,
  filesseen     INTEGER DEFAULT 0,
  itemsinserted INTEGER DEFAULT 0,
  itemsupdated  INTEGER DEFAULT 0,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_scanjobs_startedat
  ON katalog_scanjobs (startedat DESC);

CREATE TABLE IF NOT EXISTS katalog_enrichmentjobs (
  id              VARCHAR(36) NOT NULL,
  status          VARCHAR(20) NOT NULL DEFAULT 'queued',
  startedat       TIMESTAMP,
  finishedat      TIMESTAMP,
  errormessage    TEXT,
  itemsconsidered INTEGER DEFAULT 0,
  itemsenriched   INTEGER DEFAULT 0,
  itemsfailed     INTEGER DEFAULT 0,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_enrichmentjobs_startedat
  ON katalog_enrichmentjobs (startedat DESC);

CREATE TABLE IF NOT EXISTS katalog_enrichmentstatuscodes (
  code VARCHAR(20) NOT NULL,
  name VARCHAR(40),
  PRIMARY KEY (code)
);

-- Seed the five static codes. ON CONFLICT keeps the migration idempotent and
-- lets operators re-run without clobbering the names.
INSERT INTO katalog_enrichmentstatuscodes (code, name) VALUES
  ('pending',     'Pending'),
  ('in_progress', 'In Progress'),
  ('done',        'Done'),
  ('failed',      'Failed'),
  ('not_found',   'Not Found')
ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name;
