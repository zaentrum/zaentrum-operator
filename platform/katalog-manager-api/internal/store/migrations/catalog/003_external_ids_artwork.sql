-- catalog schema 003: external metadata ids + artwork (pointers and bytes).
--
-- Renamed from CAP physical tables:
--   com_nalet_katalog_itemexternalids -> katalog_itemexternalids
--   com_nalet_katalog_itemartwork     -> katalog_itemartwork
--   com_nalet_katalog_itemartworkdata -> katalog_itemartworkdata
--
-- itemartwork holds remote URL pointers; itemartworkdata holds the cached
-- bytes blob served by /api/artwork/{itemId}/{kind}. Both are keyed by
-- (item_id, kind) where kind is poster | backdrop | logo | thumbnail.

CREATE TABLE IF NOT EXISTS katalog_itemexternalids (
  id         VARCHAR(36)  NOT NULL,
  item_id    VARCHAR(36)  NOT NULL,
  source     VARCHAR(30)  NOT NULL,
  externalid VARCHAR(120) NOT NULL,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemexternalids_item
  ON katalog_itemexternalids (item_id);
-- Reverse lookup: "which item carries this tmdb/imdb id?".
CREATE INDEX IF NOT EXISTS idx_itemexternalids_source
  ON katalog_itemexternalids (source, externalid);

CREATE TABLE IF NOT EXISTS katalog_itemartwork (
  id      VARCHAR(36)   NOT NULL,
  item_id VARCHAR(36)   NOT NULL,
  kind    VARCHAR(20)   NOT NULL,
  url     VARCHAR(2048) NOT NULL,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemartwork_item
  ON katalog_itemartwork (item_id);

CREATE TABLE IF NOT EXISTS katalog_itemartworkdata (
  id          VARCHAR(36) NOT NULL,
  item_id     VARCHAR(36) NOT NULL,
  kind        VARCHAR(20) NOT NULL,
  contenttype VARCHAR(80) NOT NULL,
  bytes       BYTEA,
  fetchedat   TIMESTAMP,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemartworkdata_item
  ON katalog_itemartworkdata (item_id);
-- The artwork endpoint looks up by (item_id, kind).
CREATE INDEX IF NOT EXISTS idx_itemartworkdata_item_kind
  ON katalog_itemartworkdata (item_id, kind);
