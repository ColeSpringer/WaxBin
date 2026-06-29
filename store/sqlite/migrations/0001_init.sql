-- WaxBin schema v1 (Gate A0 minimal core).
-- Forward-only, transactional. The full storage core (genre/mood entities,
-- release_group, rollups, FTS, provenance, ...) arrives in later migrations.

-- Registered library roots. A file belongs to exactly one library; roots are
-- validated non-overlapping at config time.
CREATE TABLE library (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  root         BLOB    NOT NULL UNIQUE,  -- raw OS path bytes
  display_root TEXT    NOT NULL,
  mode         TEXT    NOT NULL,         -- managed | in-place
  profile      TEXT    NOT NULL DEFAULT 'waxbin-native',
  created_at   INTEGER NOT NULL          -- unix nanoseconds
);

-- Files on disk: audio + sidecars. Paths are BLOBs (non-UTF8 safe) with a
-- display string alongside.
CREATE TABLE file (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  library_id   INTEGER NOT NULL REFERENCES library(id) ON DELETE CASCADE,
  path         BLOB    NOT NULL UNIQUE,
  display_path TEXT    NOT NULL,
  rel_path     BLOB    NOT NULL,
  kind         TEXT    NOT NULL,         -- audio|image|lyrics|transcript|cue|chapters|peaks|nfo|foreign
  size         INTEGER NOT NULL,
  mtime_ns     INTEGER NOT NULL,
  content_hash TEXT    NOT NULL,
  essence_hash TEXT,
  analyzed_essence TEXT,
  analysis_version INTEGER,
  container    TEXT,
  codec        TEXT,
  duration_ms  INTEGER,
  bitrate      INTEGER,
  sample_rate  INTEGER,
  channels     INTEGER,
  bit_depth    INTEGER,
  scan_state   TEXT    NOT NULL,
  first_seen   INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL
);
CREATE INDEX file_library  ON file(library_id);
CREATE INDEX file_essence  ON file(essence_hash);
CREATE INDEX file_content  ON file(content_hash);

-- Logical item supertype. track/book/episode share this PK.
CREATE TABLE playable_item (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  kind         TEXT    NOT NULL,         -- track|book|episode
  state        TEXT    NOT NULL,         -- present|archived|remote|missing
  title        TEXT    NOT NULL,
  sort_key     TEXT    NOT NULL,
  identity_key TEXT,                     -- entity-identity key (package identity)
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX item_kind ON playable_item(kind);
CREATE INDEX item_sort ON playable_item(sort_key);
-- One logical item per (kind, identity_key); NULL keys are exempt.
CREATE UNIQUE INDEX item_identity ON playable_item(kind, identity_key)
  WHERE identity_key IS NOT NULL;

-- Music subtype.
CREATE TABLE track (
  item_id      INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  artist       TEXT    NOT NULL DEFAULT '',
  artist_sort  TEXT    NOT NULL DEFAULT '',
  album        TEXT    NOT NULL DEFAULT '',
  album_artist TEXT    NOT NULL DEFAULT '',
  track_no     INTEGER,
  disc_no      INTEGER,
  year         INTEGER,
  genre        TEXT    NOT NULL DEFAULT '',
  mbid         TEXT
);
CREATE INDEX track_artist ON track(artist_sort);
CREATE INDEX track_album  ON track(album);

-- Item <-> file graph with offsets (a file can back multiple items).
CREATE TABLE item_file (
  item_id  INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  file_id  INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  role     TEXT    NOT NULL DEFAULT 'primary',
  position INTEGER NOT NULL DEFAULT 0,
  start_ms INTEGER,
  end_ms   INTEGER,
  PRIMARY KEY (item_id, file_id, role)
);
CREATE INDEX item_file_file ON item_file(file_id);

-- Single delta vocabulary. Consumers tail this to keep caches current.
CREATE TABLE change_log (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,         -- unix nanoseconds
  entity_type TEXT    NOT NULL,
  entity_pid  TEXT    NOT NULL,
  op          TEXT    NOT NULL          -- create|update|delete
);

-- Mutating background work.
CREATE TABLE job (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  kind         TEXT    NOT NULL,
  scope        TEXT    NOT NULL,
  state        TEXT    NOT NULL,        -- running|done|failed|crashed|canceled
  owner        TEXT    NOT NULL,
  progress     REAL    NOT NULL DEFAULT 0,
  message      TEXT    NOT NULL DEFAULT '',
  error        TEXT    NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL,
  heartbeat_at INTEGER NOT NULL,
  finished_at  INTEGER
);
CREATE INDEX job_state ON job(state);

-- Scoped advisory leases: at most one mutating job per scope.
CREATE TABLE lease (
  scope        TEXT    PRIMARY KEY,
  owner        TEXT    NOT NULL,
  job_id       INTEGER REFERENCES job(id) ON DELETE CASCADE,
  acquired_at  INTEGER NOT NULL,
  heartbeat_at INTEGER NOT NULL
);

-- Organize move audit/recovery trail.
CREATE TABLE organize_journal (
  id         INTEGER PRIMARY KEY,
  pid        TEXT    NOT NULL UNIQUE,
  job_pid    TEXT    NOT NULL,
  file_id    INTEGER REFERENCES file(id) ON DELETE SET NULL,
  src        BLOB    NOT NULL,
  dst        BLOB    NOT NULL,
  state      TEXT    NOT NULL,          -- planned|committed|rolled_back
  created_at INTEGER NOT NULL
);
CREATE INDEX organize_journal_job ON organize_journal(job_pid);
