-- catalog schema 006: remote trailer link catalogue.
--
-- Renamed from CAP physical table:
--   com_nalet_katalog_itemtrailerlinks -> katalog_itemtrailerlinks
--
-- One row per known remote trailer (e.g. an upstream metadata provider's
-- /videos result). The read API exposes these so a client can play an
-- embedded trailer; the trailer-link-processing path may stamp localpath /
-- downloadedat once a trailer that ships next to the owned file is picked up
-- by the scanner. This is metadata about trailers we may already have — the
-- acquisition-side fetch tracker is intentionally NOT part of the neutral
-- schema (see SCHEMA_OUT droppedTables).

CREATE TABLE IF NOT EXISTS katalog_itemtrailerlinks (
  id           VARCHAR(36)   NOT NULL,
  createdat    TIMESTAMP,
  createdby    VARCHAR(255),
  modifiedat   TIMESTAMP,
  modifiedby   VARCHAR(255),
  item_id      VARCHAR(36)   NOT NULL,
  source       VARCHAR(20)   NOT NULL,
  site         VARCHAR(40),
  externalid   VARCHAR(120),
  url          VARCHAR(2048) NOT NULL,
  title        VARCHAR(255),
  durationsec  INTEGER,
  publishedat  TIMESTAMP,
  downloadedat TIMESTAMP,
  localpath    VARCHAR(2048),
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_trailerlinks_item_id
  ON katalog_itemtrailerlinks (item_id);
