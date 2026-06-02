-- catalog schema 004: on-disk media assets.
--
-- Renamed from CAP physical tables:
--   com_nalet_katalog_playbackassets -> katalog_playbackassets
--   com_nalet_katalog_subtitleassets -> katalog_subtitleassets
--
-- playbackassets records the actual media files the operator owns on disk
-- (the read API resolves a stream token to a `path` here). The `kind` column
-- (added in the trailer migration) distinguishes primary content from
-- trailers / samples / featurettes. subtitleassets records sidecar subtitle
-- files discovered next to the primary asset.

CREATE TABLE IF NOT EXISTS katalog_playbackassets (
  id          VARCHAR(36)   NOT NULL,
  item_id     VARCHAR(36)   NOT NULL,
  path        VARCHAR(2048) NOT NULL,
  codec       VARCHAR(40),
  resolution  VARCHAR(40),
  bitratekbps INTEGER,
  sizebytes   BIGINT,
  hash        VARCHAR(160),
  isprimary   BOOLEAN DEFAULT FALSE,
  kind        VARCHAR(20) DEFAULT 'primary',
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_playbackassets_item
  ON katalog_playbackassets (item_id);
-- The import scanner checks `WHERE path = $1` to stay idempotent.
CREATE INDEX IF NOT EXISTS idx_playbackassets_path
  ON katalog_playbackassets (path);
-- "Give me all trailers/extras for item X".
CREATE INDEX IF NOT EXISTS idx_playbackassets_kind
  ON katalog_playbackassets (item_id, kind)
  WHERE kind <> 'primary';

CREATE TABLE IF NOT EXISTS katalog_subtitleassets (
  id        VARCHAR(36)   NOT NULL,
  item_id   VARCHAR(36)   NOT NULL,
  path      VARCHAR(2048) NOT NULL,
  format    VARCHAR(10),
  lang      VARCHAR(10),
  label     VARCHAR(120),
  isdefault BOOLEAN DEFAULT FALSE,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_subtitleassets_item
  ON katalog_subtitleassets (item_id);
