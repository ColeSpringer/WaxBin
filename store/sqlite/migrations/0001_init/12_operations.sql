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
CREATE INDEX organize_journal_job  ON organize_journal(job_pid);
-- The SET NULL above is checked on every file delete; the journal grows with
-- every organize move, so without this index each pruned/trashed file row
-- rescans the whole journal.
CREATE INDEX organize_journal_file ON organize_journal(file_id);

-- Safe deletion with a same-volume trash and undo journal. A trashed file is
-- moved to a per-library trash directory and recorded here; the logical item
-- is preserved (archived when it loses its last file). Restore moves the file
-- back and re-scans it. Pruning/permanent deletion bypasses the trash to
-- reclaim space and records no row. A NULL restored_at means still in the
-- trash.
CREATE TABLE trash (
  id            INTEGER PRIMARY KEY,
  pid           TEXT    NOT NULL UNIQUE,
  library_id    INTEGER REFERENCES library(id) ON DELETE CASCADE,
  item_pid      TEXT    NOT NULL DEFAULT '',  -- the item the file backed (for reporting)
  orig_path     BLOB    NOT NULL,             -- where the file was, raw bytes
  orig_display  TEXT    NOT NULL,
  trash_path    BLOB    NOT NULL,             -- where it now lives, raw bytes
  trash_display TEXT    NOT NULL,
  reason        TEXT    NOT NULL DEFAULT 'user', -- user|prune|dedup|organize
  size          INTEGER NOT NULL DEFAULT 0,
  trashed_at    INTEGER NOT NULL,             -- unix nanoseconds
  restored_at   INTEGER                       -- NULL = still in trash
);
CREATE INDEX trash_active ON trash(restored_at);
CREATE INDEX trash_item   ON trash(item_pid);

-- Staging/inbox import batches. Each import of a staging folder into a managed
-- library is recorded with its source attribution and a tally of imported/
-- duplicate/quarantined/errored files, so an import is auditable and
-- reviewable after the fact.
CREATE TABLE import_batch (
  id          INTEGER PRIMARY KEY,
  pid         TEXT    NOT NULL UNIQUE,
  source      TEXT    NOT NULL,                 -- staging folder / attribution
  library_id  INTEGER REFERENCES library(id) ON DELETE SET NULL,
  state       TEXT    NOT NULL,                 -- running|done|failed
  imported    INTEGER NOT NULL DEFAULT 0,
  duplicates  INTEGER NOT NULL DEFAULT 0,
  quarantined INTEGER NOT NULL DEFAULT 0,
  errored     INTEGER NOT NULL DEFAULT 0,
  bytes       INTEGER NOT NULL DEFAULT 0,       -- bytes brought in
  started_at  INTEGER NOT NULL,                 -- unix nanoseconds
  finished_at INTEGER
);
CREATE INDEX import_batch_started ON import_batch(started_at);

-- Provider credentials and other named secrets. Values are stored plaintext;
-- they are never logged or written to a logical export. A full DB backup is a
-- byte copy and therefore includes this table, so backup exposes a redaction
-- option for copies that leave the host.
CREATE TABLE secret (
  key        TEXT    PRIMARY KEY,
  value      TEXT    NOT NULL,
  updated_at INTEGER NOT NULL  -- unix nanoseconds
);

-- Orphan-entity GC grace tracking: when an entity (artist/release_group/album/
-- genre/series) was first observed childless, so the manual GC only sweeps an
-- entity that has stayed orphaned past a grace window (a two-run / time-based
-- confirmation). This is the safety backstop to the scanner's survival gate: a
-- transient reconciliation blip cannot immediately destroy MusicBrainz
-- enrichment or operator merges.
CREATE TABLE orphan_candidate (
  entity_type TEXT    NOT NULL,          -- artist | release_group | album | genre | series
  entity_id   INTEGER NOT NULL,
  first_seen  INTEGER NOT NULL,          -- unix ns first observed orphaned
  PRIMARY KEY (entity_type, entity_id)
);

-- Persisted per-file diagnostics. origin is the writer that produced the row
-- (scan | organize | replaygain), not the phase it happened in. It is part of
-- the primary key, so each writer replaces only its own rows, and cross-writer
-- isolation is a property of the schema rather than of a delete predicate.
CREATE TABLE file_diagnostic (
  file_id  INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  origin   TEXT    NOT NULL,            -- scan | organize | replaygain
  code     TEXT    NOT NULL,
  severity TEXT    NOT NULL,            -- info | warn | error
  tag_key  TEXT    NOT NULL DEFAULT '',
  detail   TEXT    NOT NULL DEFAULT '',
  seen_at  INTEGER NOT NULL,
  PRIMARY KEY (file_id, origin, code, tag_key)
);
CREATE INDEX file_diagnostic_code ON file_diagnostic(code);
