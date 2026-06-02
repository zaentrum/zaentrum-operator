-- catalog schema 002: genre / people / tag dimensions and their item links.
--
-- Renamed from CAP physical tables:
--   com_nalet_katalog_genres      -> katalog_genres
--   com_nalet_katalog_itemgenres  -> katalog_itemgenres
--   com_nalet_katalog_people      -> katalog_people
--   com_nalet_katalog_itempeople  -> katalog_itempeople
--   com_nalet_katalog_itemtags    -> katalog_itemtags
--
-- The link tables carry the FK columns the read path joins on
-- (item_id, genre_id, person_id) plus the cast `role` discriminator the
-- similarity scorer filters by (role = 'actor').

CREATE TABLE IF NOT EXISTS katalog_genres (
  id   VARCHAR(36) NOT NULL,
  name VARCHAR(80) NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS katalog_itemgenres (
  id       VARCHAR(36) NOT NULL,
  item_id  VARCHAR(36) NOT NULL,
  genre_id VARCHAR(36) NOT NULL,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemgenres_item  ON katalog_itemgenres (item_id);
CREATE INDEX IF NOT EXISTS idx_itemgenres_genre ON katalog_itemgenres (genre_id);

CREATE TABLE IF NOT EXISTS katalog_people (
  id   VARCHAR(36)  NOT NULL,
  name VARCHAR(255) NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS katalog_itempeople (
  id        VARCHAR(36) NOT NULL,
  item_id   VARCHAR(36) NOT NULL,
  person_id VARCHAR(36) NOT NULL,
  role      VARCHAR(40) NOT NULL,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itempeople_item   ON katalog_itempeople (item_id);
CREATE INDEX IF NOT EXISTS idx_itempeople_person ON katalog_itempeople (person_id);
-- The similarity scorer keys on (person_id, role='actor'); a partial index
-- keeps that seek tight.
CREATE INDEX IF NOT EXISTS idx_itempeople_actor
  ON katalog_itempeople (person_id)
  WHERE role = 'actor';

CREATE TABLE IF NOT EXISTS katalog_itemtags (
  id      VARCHAR(36)  NOT NULL,
  item_id VARCHAR(36)  NOT NULL,
  tag     VARCHAR(120) NOT NULL,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_itemtags_item ON katalog_itemtags (item_id);
