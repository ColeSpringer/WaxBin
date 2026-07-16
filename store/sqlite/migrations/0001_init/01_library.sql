-- WaxBin catalog schema, the v1 baseline (migration 0001_init).
-- Every file in this directory belongs to one migration applied in a single
-- transaction, in filename order: the numeric prefix puts FK targets ahead of
-- their referrers and carries no version meaning. Post-1.0 schema changes land
-- as new NNNN_*.sql migrations.

-- Registered library roots. A file belongs to exactly one library; roots are
-- validated non-overlapping at config time. media is what a managed root
-- holds, so organize/import can route an item to the type-matching library by
-- its kind ('mixed' keeps the content-classified single-tree behavior).
CREATE TABLE library (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  root         BLOB    NOT NULL UNIQUE,  -- raw OS path bytes
  display_root TEXT    NOT NULL,
  mode         TEXT    NOT NULL,         -- managed | in-place
  media        TEXT    NOT NULL DEFAULT 'mixed',  -- music|audiobook|podcast|mixed
  profile      TEXT    NOT NULL DEFAULT 'waxbin-native',
  created_at   INTEGER NOT NULL          -- unix nanoseconds
);

-- Files on disk: audio + sidecars. Paths are BLOBs (non-UTF8 safe) with a
-- display string alongside. analyzed_essence/analysis_version stamp what the
-- analyze pass last measured, so derived data from superseded audio reads as
-- absent. diag_version is the rule-set version this file's diagnostics were
-- last derived under (0 = never derived; only a full-path scan stamps it), so
-- the audit reports coverage rather than letting an absence of rows read as a
-- clean library.
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
  diag_version INTEGER NOT NULL DEFAULT 0,
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
CREATE INDEX file_library ON file(library_id);
CREATE INDEX file_essence ON file(essence_hash);
CREATE INDEX file_content ON file(content_hash);
-- FilesNeedingAnalysis filters to audio files and pages by (rel_path, id); this
-- partial index lets the planner seek and scan in order instead of full-scanning
-- and filesorting the whole file table on every analyze run. The index
-- implicitly carries the rowid (id), supporting the (rel_path, id) tiebreak.
CREATE INDEX file_audio_relpath ON file(rel_path) WHERE kind = 'audio';

-- Incremental-scan fast path: the on-disk state of each sidecar (a .lrc/.cue/
-- chapter file or a directory cover) beside an audio file, so a rescan can
-- os.Stat-compare it and re-parse only when it changed. size+mtime_ns are the
-- cheap comparison; hash is the content digest recorded when the sidecar was
-- last parsed; missing marks a sidecar observed before but now gone from disk.
-- This file-observation state stays out of the user-facing lyrics/art tables.
CREATE TABLE file_aux_state (
  file_id  INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  kind     TEXT    NOT NULL,          -- lrc | cover | cue | chapters
  path     BLOB    NOT NULL,
  size     INTEGER NOT NULL DEFAULT 0,
  mtime_ns INTEGER NOT NULL DEFAULT 0,
  hash     TEXT    NOT NULL DEFAULT '',
  missing  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (file_id, kind)
);
