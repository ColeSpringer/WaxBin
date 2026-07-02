-- WaxBin schema v22: incremental-scan fast-path support.
-- file_aux_state records the on-disk state of each sidecar (a .lrc/.cue/chapter
-- file or a directory cover) beside an audio file, so a rescan can os.Stat-compare
-- it and re-parse only when it changed, keeping this file-observation state out of
-- the user-facing lyrics/art tables. chapter.source marks where a chapter row came
-- from so precedence is enforceable (a re-scan of embedded chapters must not
-- overwrite richer cue/podcast-URL chapters).

-- One row per (audio file, sidecar kind). size+mtime_ns are the cheap comparison the
-- scanner stats against; hash is the content digest recorded when the sidecar was
-- last parsed; missing marks a sidecar observed before but now gone from disk.
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

-- Origin of a chapter row: embedded (from audio tags), cue (external .cue/chapter
-- file), or podcast_url (Podcasting-2.0 chapters JSON). A re-scan replaces only the
-- rows for the source it is writing, so a poorer source cannot clobber a richer one.
ALTER TABLE chapter ADD COLUMN source TEXT NOT NULL DEFAULT 'embedded';
