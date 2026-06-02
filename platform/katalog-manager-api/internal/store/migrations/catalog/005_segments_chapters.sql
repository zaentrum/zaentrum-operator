-- catalog schema 005: skippable segments + structural chapter atoms.
--
-- Renamed from CAP physical tables:
--   com_nalet_katalog_mediasegments -> katalog_mediasegments
--   com_nalet_katalog_itemchapters  -> katalog_itemchapters
--
-- mediasegments holds skippable segments aligned with the upstream
-- intro-database vocabulary (intro | recap | credits | preview), written by
-- the analyzer. itemchapters holds the file-internal chapter atoms ffprobe
-- extracts (non-skippable, free-form titles) — split out from mediasegments
-- so the two distinct concepts no longer share a `kind` column.

CREATE TABLE IF NOT EXISTS katalog_mediasegments (
  id         VARCHAR(36) NOT NULL,
  createdat  TIMESTAMP,
  createdby  VARCHAR(255),
  modifiedat TIMESTAMP,
  modifiedby VARCHAR(255),
  item_id    VARCHAR(36) NOT NULL,
  kind       VARCHAR(20) NOT NULL,
  startms    BIGINT      NOT NULL,
  endms      BIGINT      NOT NULL,
  source     VARCHAR(30) NOT NULL,
  confidence DECIMAL(3, 2),
  label      VARCHAR(120),
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_mediasegments_item_id
  ON katalog_mediasegments (item_id);

CREATE TABLE IF NOT EXISTS katalog_itemchapters (
  id         VARCHAR(36)  NOT NULL,
  createdat  TIMESTAMP,
  createdby  VARCHAR(255),
  modifiedat TIMESTAMP,
  modifiedby VARCHAR(255),
  item_id    VARCHAR(36)  NOT NULL,
  startms    BIGINT       NOT NULL,
  endms      BIGINT       NOT NULL,
  title      VARCHAR(120),
  ordinal    INTEGER,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemchapters_item_id
  ON katalog_itemchapters (item_id);
