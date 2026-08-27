-- Sparse field provenance and user locks. A row exists only when a field is
-- not plain tag-sourced: it was edited by a user, written by enrichment or
-- organize, or locked. The absence of a row means "from the tag, unlocked"
-- (the common case), so the table stays sparse. Writers (organize tag
-- write-back, enrichment) consult locked before overwriting, so curated data
-- is protected. provider is the stable provider id ("musicbrainz", "lrclib")
-- that set an enrichment value, so a consumer can attribute a field and work
-- out a metadata conflict; it is NULL for a tag or user edit, which have no
-- external provider.
--
-- source carries the scalar vocabulary on a scalar field. The artifact lock rows
-- ("art", "lyrics", "chapters") are the exception: they hold no value of their own, so
-- they carry the artifact's own source, which may be 'sidecar' or 'feed' as well. That
-- row is what FieldProvenance and ArtRoles report when the artifact was cleared and
-- locked and there is nothing else left to attribute.
CREATE TABLE field_provenance (
  item_id    INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  field      TEXT    NOT NULL,        -- canonical field name (title|artist|album|...)
  source     TEXT    NOT NULL,        -- tag|user|enrichment|organize, plus the artifact values on an artifact lock row
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
-- rows onto the survivor explicitly, with locked-wins. The lock is what guards the
-- unconditional entity enrich writes (release_group.type, a fetched cover) and the
-- user overrides. The name is kept distinct from the item-scoped field_provenance so
-- the two do not get confused, since they sit close together.
--
-- The scalar fields only ever name the three editable entities, but the rows are
-- polymorphic like art_map's, and the 'art' field can name any art entity type: a
-- podcast, a playlist and a genre all hold a cover a user can choose and lock, and
-- none of them is scalar-editable.
--
-- provider mirrors field_provenance.provider, and is nullable for the same reason: this
-- table keeps a sparse-row contract, so a row with no external provider stores NULL
-- rather than an empty string. art_map's is NOT NULL DEFAULT '' because it keeps no
-- such contract. Without the column an entity edit carrying a source of 'enrichment'
-- would lose the name of the service that supplied the value.
CREATE TABLE entity_curation (
  entity_type TEXT    NOT NULL,        -- artist|release_group|album, plus any art entity for 'art'
  entity_id   INTEGER NOT NULL,
  field       TEXT    NOT NULL,        -- sort|mbid|type|barcode|label|catalog_number|media|country|art
  source      TEXT    NOT NULL,        -- user|enrichment, plus the artifact values on the 'art' lock row
  provider    TEXT,                    -- enrichment provider id, when source = enrichment
  locked      INTEGER NOT NULL DEFAULT 0,
  value       TEXT,                    -- the curated value, when set by a user edit
  updated_at  INTEGER NOT NULL,        -- unix nanoseconds
  PRIMARY KEY (entity_type, entity_id, field)
);
CREATE INDEX entity_curation_locked ON entity_curation(entity_type, entity_id) WHERE locked = 1;

-- Custom (non-standard) tags preserved per item: the tag frames a file carries that
-- WaxBin's typed model does not map to a column, plus user-set custom tags. key is a
-- canonical uppercase-ASCII tag key (so KEY and key dedup to one), and position
-- preserves the order of a multi-valued tag. The set is replaced wholesale on a scan
-- (per key, unless that key is locked under field_provenance 'tag.<KEY>') or by the
-- SetItemTag edit. It stays sparse: an item with no extra tags has no rows.
CREATE TABLE item_tag (
  item_id  INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  key      TEXT    NOT NULL,        -- canonical uppercase tag key (e.g. MOOD, KEY)
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
--
-- source and provider are the art_map vocabulary, so a consumer draws one mark
-- under a cover and a lyric alike. The column used to hold three vocabularies at
-- once ('lrc' and 'embedded' from the scan, 'user' from a curation edit, and a
-- provider id such as 'lrclib' from enrichment), which left no way to tell a
-- provider id from a sidecar kind. There is no source_url: LRCLIB's /api/get is a
-- query endpoint, not a stable page to cite, so there would be nothing to record.
CREATE TABLE lyrics (
  item_id    INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  source     TEXT    NOT NULL,           -- tag (USLT/SYLT) | sidecar (.lrc) | user | enrichment
  provider   TEXT    NOT NULL DEFAULT '',-- lyrics provider id, when source = enrichment
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
-- genre); the other roles resolve at their own level, as does every role on a
-- playlist (a playlist has no ancestry to inherit from). Orphan rows left by an
-- entity deletion are cleaned by the art GC, which then drops the
-- now-unreferenced source images and (by cascade) their thumbnails.
--
-- The provenance columns live here rather than on art_source because that store is
-- content-addressed: one JPEG both scraped from the Cover Art Archive and embedded in
-- a file is a single row, backing a tag cover on one album and a fetched cover on
-- another, and only the mapping knows which. provider is NOT NULL DEFAULT '' where
-- field_provenance.provider is nullable because this table keeps no sparse-row
-- contract. A sidecar cover's on-disk path is not recorded in source_url; it is the
-- AuxCover observation in file_aux_state.
--
-- source_hash is always derivable: the store fills a missing content address (and a
-- missing format or dimensions) from the image bytes rather than dropping the write, so
-- a producer that hands over bytes alone still gets its cover stored. Do not reinstate a
-- guard that discards such an image; it discarded them silently.
CREATE TABLE art_map (
  entity_type TEXT    NOT NULL,                     -- track|album|release_group|artist|genre|episode|podcast|playlist
  entity_id   INTEGER NOT NULL,
  source_hash TEXT    NOT NULL REFERENCES art_source(hash) ON DELETE CASCADE,
  role        TEXT    NOT NULL DEFAULT 'front',     -- front|back|disc|booklet|background
  source      TEXT    NOT NULL,                     -- tag|sidecar|user|enrichment|feed|generated
  provider    TEXT    NOT NULL DEFAULT '',          -- metadata provider id, when source = enrichment
  source_url  TEXT    NOT NULL DEFAULT '',          -- fetch URL, for an enrichment or feed cover
  updated_at  INTEGER NOT NULL,                     -- unix nanoseconds
  PRIMARY KEY (entity_type, entity_id, role)
);
-- Named for the column it indexes, so it is not read as an index on source.
CREATE INDEX art_map_source_hash ON art_map(source_hash);

-- Size-negotiated thumbnails generated on demand, keyed by (source hash, ladder
-- rung); derived data, reference-counted against art_source and garbage-collected
-- when its source is unreferenced.
CREATE TABLE thumb_cache (
  source_hash TEXT    NOT NULL REFERENCES art_source(hash) ON DELETE CASCADE,
  size        INTEGER NOT NULL,           -- ladder rung in px (art.Rung)
  format      TEXT    NOT NULL,           -- jpeg|png
  width       INTEGER NOT NULL,
  height      INTEGER NOT NULL,
  data        BLOB    NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (source_hash, size)
);
