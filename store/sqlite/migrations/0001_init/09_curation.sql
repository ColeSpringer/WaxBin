-- Sparse field provenance and user locks. A row exists only when a field is
-- not plain tag-sourced: it was edited by a user, written by enrichment or
-- organize, or locked. The absence of a row means "from the tag, unlocked"
-- (the common case), so the table stays sparse. Writers (organize tag
-- write-back, enrichment) consult locked before overwriting, so curated data
-- is protected. provider is the stable provider id ("musicbrainz", "lrclib")
-- that set an enrichment value, so a consumer can attribute a field and work
-- out a metadata conflict; it is NULL for a tag or user edit, which have no
-- external provider.
CREATE TABLE field_provenance (
  item_id    INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  field      TEXT    NOT NULL,        -- canonical field name (title|artist|album|...)
  source     TEXT    NOT NULL,        -- tag|user|enrichment|organize
  provider   TEXT,                    -- enrichment provider id, when source = enrichment
  locked     INTEGER NOT NULL DEFAULT 0,
  value      TEXT,                    -- the curated value, when set by a user edit
  updated_at INTEGER NOT NULL,        -- unix nanoseconds
  PRIMARY KEY (item_id, field)
);
CREATE INDEX field_provenance_locked ON field_provenance(item_id) WHERE locked = 1;

-- Structured lyrics (synced + unsynced), keyed by item. WaxBin parses a
-- sibling .lrc sidecar directly (the authoritative source when present) and
-- reads embedded USLT (unsynced) and SYLT (synced) tags through WaxLabel at
-- scan time. The DB row is authoritative for reads; synced lines are stored as
-- JSON [{ms,text}] in time order. A row exists only when the file carried some
-- lyric content, so the table stays sparse.
CREATE TABLE lyrics (
  item_id    INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  source     TEXT    NOT NULL,           -- 'lrc' (sidecar) | 'embedded'
  synced     INTEGER NOT NULL DEFAULT 0, -- 1 when timed lines are present
  unsynced   TEXT,                       -- plain unsynchronized text (USLT)
  lines      TEXT,                       -- JSON [{"ms":N,"text":"..."}], time-ordered
  updated_at INTEGER NOT NULL
);

-- The art resolution store: a content-addressed store of source images (dedup
-- by hash, so the one album cover embedded in every track is stored once).
CREATE TABLE art_source (
  hash       TEXT    PRIMARY KEY,         -- content hash of the image bytes
  format     TEXT    NOT NULL,            -- jpeg|png|webp|gif|...
  width      INTEGER NOT NULL DEFAULT 0,
  height     INTEGER NOT NULL DEFAULT 0,
  size       INTEGER NOT NULL,            -- byte length of data
  data       BLOB    NOT NULL,
  created_at INTEGER NOT NULL
);

-- Polymorphic entity art map: entity_type selects which table entity_id refers
-- to, so there is no single FK to enforce. The resolver walks the fallback
-- chain track -> album -> release_group -> artist -> genre. Orphan rows left
-- by an entity deletion are cleaned by the art GC, which then drops the
-- now-unreferenced source images and (by cascade) their thumbnails. Lower
-- priority wins within one entity+role.
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

-- Size-negotiated thumbnails generated on demand, keyed by (source hash, max
-- dimension); derived data, reference-counted against art_source and
-- garbage-collected when its source is unreferenced.
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
