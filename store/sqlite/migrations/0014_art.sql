-- WaxBin schema v14: the art resolution store and derived thumbnail cache.
-- art_source is a content-addressed store of source images (dedup by hash, so the
-- one album cover embedded in every track is stored once). art_map attaches a
-- source image to an entity (track|album|release_group|artist|genre) with a role,
-- so the resolver walks the fallback chain track -> album -> release_group ->
-- artist -> genre. thumb_cache holds size-negotiated thumbnails generated on
-- demand, keyed by (source hash, max dimension); it is derived data,
-- reference-counted against art_source and garbage-collected when its source is
-- unreferenced.

CREATE TABLE art_source (
  hash       TEXT    PRIMARY KEY,         -- content hash of the image bytes
  format     TEXT    NOT NULL,            -- jpeg|png|webp|gif|...
  width      INTEGER NOT NULL DEFAULT 0,
  height     INTEGER NOT NULL DEFAULT 0,
  size       INTEGER NOT NULL,            -- byte length of data
  data       BLOB    NOT NULL,
  created_at INTEGER NOT NULL
);

-- Polymorphic entity art map: entity_type selects which table entity_id refers to,
-- so there is no single FK to enforce. Orphan rows left by an entity deletion are
-- cleaned by the art GC, which then drops the now-unreferenced source images and
-- (by cascade) their thumbnails. lower priority wins within one entity+role.
CREATE TABLE art_map (
  entity_type TEXT    NOT NULL,                     -- track|album|release_group|artist|genre
  entity_id   INTEGER NOT NULL,
  source_hash TEXT    NOT NULL REFERENCES art_source(hash) ON DELETE CASCADE,
  role        TEXT    NOT NULL DEFAULT 'front',     -- front|back|artist|...
  priority    INTEGER NOT NULL DEFAULT 0,           -- lower wins within an entity
  PRIMARY KEY (entity_type, entity_id, source_hash)
);
CREATE INDEX art_map_entity ON art_map(entity_type, entity_id);
CREATE INDEX art_map_source ON art_map(source_hash);

CREATE TABLE thumb_cache (
  source_hash TEXT    NOT NULL REFERENCES art_source(hash) ON DELETE CASCADE,
  size        INTEGER NOT NULL,           -- requested max dimension in px
  format      TEXT    NOT NULL,           -- jpeg|png
  width       INTEGER NOT NULL,
  height      INTEGER NOT NULL,
  data        BLOB    NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (source_hash, size)
);
