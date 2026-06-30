-- WaxBin schema v10: safe deletion with a same-volume trash and undo journal.
-- A trashed file is moved to a per-library trash directory and recorded here; the
-- logical item is preserved (archived when it loses its last file). Restore moves
-- the file back and re-scans it. Pruning/permanent deletion bypasses the trash to
-- reclaim space and records no row. A NULL restored_at means still in the trash.

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
