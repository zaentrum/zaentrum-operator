-- catalog schema 001: the canonical media item table.
--
-- Movies, series, seasons, episodes, albums, tracks and books all live in one
-- table with a `type` discriminator and a `parent_id` self-reference (season
-- -> series, episode -> season, track -> album).
--
-- Renamed from the CAP-generated physical table com_nalet_katalog_items ->
-- katalog_items. Column set is the *current* shape: the legacy single-column
-- status indicators (enrichmentstatus / enrichedat / analyzerstatus /
-- analyzerstartedat / analyzererror) were retired in favour of the per-step
-- audit in katalog_itemprocessingsteps, so they are not recreated here.
--
-- Postgres folds unquoted identifiers to lowercase; every identifier below is
-- written unquoted so the physical column names match what the read path
-- (katalog-api) and the write path (the import scanner) expect.

CREATE TABLE IF NOT EXISTS katalog_items (
  id            VARCHAR(36)  NOT NULL,
  createdat     TIMESTAMP,
  createdby     VARCHAR(255),
  modifiedat    TIMESTAMP,
  modifiedby    VARCHAR(255),
  type          VARCHAR(20)  NOT NULL,
  title         VARCHAR(255) NOT NULL,
  sorttitle     VARCHAR(255),
  year          INTEGER,
  description   TEXT,
  rating        DECIMAL(3, 1),
  durationms    BIGINT,
  parent_id     VARCHAR(36),
  seasonnumber  INTEGER,
  episodenumber INTEGER,
  tagline       VARCHAR(500),
  PRIMARY KEY (id)
);

-- Full-text search vector over title + description. The read API's list query
-- filters with `search_vector @@ websearch_to_tsquery('simple', $q)`, so this
-- column and its GIN index are part of the read contract. Generated (STORED)
-- so writers never have to maintain it by hand.
ALTER TABLE katalog_items
  ADD COLUMN IF NOT EXISTS search_vector tsvector
  GENERATED ALWAYS AS (
    to_tsvector('simple',
      coalesce(title, '') || ' ' || coalesce(description, ''))
  ) STORED;

CREATE INDEX IF NOT EXISTS idx_items_search_vector
  ON katalog_items USING GIN (search_vector);

-- Self-reference lookups: "give me the children of this series/season/album".
CREATE INDEX IF NOT EXISTS idx_items_parent_id
  ON katalog_items (parent_id)
  WHERE parent_id IS NOT NULL;

-- Episode browse ordering is by (parent, season, episode).
CREATE INDEX IF NOT EXISTS idx_items_episode_coords
  ON katalog_items (parent_id, seasonnumber, episodenumber)
  WHERE type = 'episode';

-- Default browse sort + the "newest" rail.
CREATE INDEX IF NOT EXISTS idx_items_sorttitle ON katalog_items (sorttitle);
CREATE INDEX IF NOT EXISTS idx_items_type      ON katalog_items (type);
CREATE INDEX IF NOT EXISTS idx_items_createdat ON katalog_items (createdat DESC);
