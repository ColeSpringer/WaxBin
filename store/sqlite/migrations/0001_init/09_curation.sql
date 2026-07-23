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

-- Sparse entity-level curation and locks: the entity-scoped analogue of
-- field_provenance for the shared entities (artist, release group, album) that no
-- single item owns. A row exists only when a user set an entity field (a sort-name
-- override, an identifier) or a value otherwise needs protecting from an enrichment
-- overwrite, so the table stays sparse. entity_type selects which table entity_id
-- refers to, so there is no single FK (like art_map); a merge re-points the loser's
-- rows onto the survivor explicitly, with locked-wins. The lock is what guards the one
-- unconditional entity enrich write (release_group.type) and the user overrides. The
-- name is kept distinct from the item-scoped field_provenance so the two do not get
-- confused, since they sit close together.
CREATE TABLE entity_curation (
  entity_type TEXT    NOT NULL,        -- artist|release_group|album
  entity_id   INTEGER NOT NULL,
  field       TEXT    NOT NULL,        -- sort|mbid|type|barcode|label|catalog_number
  source      TEXT    NOT NULL,        -- user|enrichment
  locked      INTEGER NOT NULL DEFAULT 0,
  value       TEXT,                    -- the curated value, when set by a user edit
  updated_at  INTEGER NOT NULL,        -- unix nanoseconds
  PRIMARY KEY (entity_type, entity_id, field)
);
CREATE INDEX entity_curation_locked ON entity_curation(entity_type, entity_id) WHERE locked = 1;

-- Custom (non-standard) tags preserved per item: the tag frames a file carries that
-- WaxBin's typed model does not map to a column, plus user-set custom tags. key is a
-- canonical uppercase-ASCII tag key (so BPM and bpm dedup to one), and position
-- preserves the order of a multi-valued tag. The set is replaced wholesale on a scan
-- (per key, unless that key is locked under field_provenance 'tag.<KEY>') or by the
-- SetItemTag edit. It stays sparse: an item with no extra tags has no rows.
CREATE TABLE item_tag (
  item_id  INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  key      TEXT    NOT NULL,        -- canonical uppercase tag key (e.g. MOOD, BPM)
  value    TEXT    NOT NULL,
  position INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, key, position)
);
CREATE INDEX item_tag_item ON item_tag(item_id);
-- Serves the catalog-wide, key-driven reads: the tag.<KEY> facet's COUNT(DISTINCT
-- pi.id) grouped by value and TagKeys' COUNT(DISTINCT item_id) grouped by key. item_tag
-- is a rowid table, so a secondary index appends the hidden rowid (not the PK columns);
-- naming item_id explicitly makes both those aggregates index-only (covering) for one
-- extra INTEGER per row on a sparse table. Because it also carries value, this index
-- covers the per-item tag.<KEY> EXISTS predicate too: an equality check resolves to an
-- exact (key, value, item_id) seek and a presence check to a (key) covering seek, so no
-- tag query needs a row fetch. (The (item_id, key, position) PK still uniquely keys a
-- tag's rows for writes.)
CREATE INDEX item_tag_key ON item_tag(key, value, item_id);

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
-- to, so there is no single FK to enforce. An entity holds at most one image
-- per role (the primary key), so a slot is replaced by delete-then-insert and
-- a lookup needs no ordering. Only the front role participates in the
-- resolver's fallback chain (track -> album -> release_group -> artist ->
-- genre); the other roles resolve at their own level. Orphan rows left by an
-- entity deletion are cleaned by the art GC, which then drops the
-- now-unreferenced source images and (by cascade) their thumbnails.
CREATE TABLE art_map (
  entity_type TEXT    NOT NULL,                     -- track|album|release_group|artist|genre|episode|podcast
  entity_id   INTEGER NOT NULL,
  source_hash TEXT    NOT NULL REFERENCES art_source(hash) ON DELETE CASCADE,
  role        TEXT    NOT NULL DEFAULT 'front',     -- front|back|disc|booklet|background
  PRIMARY KEY (entity_type, entity_id, role)
);
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
