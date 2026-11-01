-- Logical item supertype. track/book/episode share this PK.
CREATE TABLE playable_item (
  id           INTEGER PRIMARY KEY,
  pid          TEXT    NOT NULL UNIQUE,
  kind         TEXT    NOT NULL,         -- track|book|episode
  state        TEXT    NOT NULL,         -- present|archived|remote|missing (model.ItemStates)
  title        TEXT    NOT NULL,
  title_sort   TEXT    NOT NULL DEFAULT '',  -- the sort spelling the file states (a book's ALBUMSORT), '' when none
  sort_key     TEXT    NOT NULL,
  title_key    TEXT    NOT NULL DEFAULT '',  -- the title alone folded (model.SortKey), what a lookup by title compares
  identity_key TEXT,                     -- entity-identity key (package identity)
  -- The track's or book's year, 0 when it has none and for an episode, kept here so the
  -- newest list walks an index; upsertTrack and upsertBook write it beside the year.
  release_year INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX item_sort ON playable_item(sort_key);
CREATE INDEX item_title_key ON playable_item(title_key);
-- The recently-added and newest orders, the pid tiebreak included, so a page is a walk.
-- A list scoped to one kind walks the kind-led twin of its order, which a lookup by kind
-- alone also uses.
CREATE INDEX item_created ON playable_item(created_at, pid);
CREATE INDEX item_release_year ON playable_item(release_year, pid) WHERE kind <> 'episode';
CREATE INDEX item_kind_sort ON playable_item(kind, sort_key);
CREATE INDEX item_kind_created ON playable_item(kind, created_at, pid);
CREATE INDEX item_kind_release_year ON playable_item(kind, release_year, pid) WHERE kind <> 'episode';
-- One logical item per (kind, identity_key); NULL keys are exempt.
CREATE UNIQUE INDEX item_identity ON playable_item(kind, identity_key)
  WHERE identity_key IS NOT NULL;

-- Where an item a kind change's write folded away went (absorbed = 1) and which item's
-- file an item it made came from (absorbed = 0), recorded under the change's job in the
-- write's own transaction, so a change a crash or a cancel cuts off can still say both. A
-- change that finishes drops its rows. Pids, not ids: a folded item is deleted.
CREATE TABLE item_lineage (
  pid       TEXT    NOT NULL,
  absorbed  INTEGER NOT NULL,
  job_pid   TEXT    NOT NULL,
  other_pid TEXT    NOT NULL,
  at        INTEGER NOT NULL,          -- unix nanoseconds
  PRIMARY KEY (pid, absorbed, job_pid)
) WITHOUT ROWID;
CREATE INDEX item_lineage_job ON item_lineage(job_pid, absorbed, other_pid);

-- Item <-> file graph with offsets (a file can back multiple items).
-- start_frames/end_frames are the virtual-track window in CD frames (75/sec), the
-- unit the .cue sheet that carved it is written in, and NULL for a whole-file edge.
-- Frames rather than milliseconds because the window is the track's content
-- identity: every rate served divides 75 exactly, so a frame is an exact sample,
-- while a millisecond quantizes 50 of every 75 frames away.
CREATE TABLE item_file (
  item_id      INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  file_id      INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  role         TEXT    NOT NULL DEFAULT 'primary',
  position     INTEGER NOT NULL DEFAULT 0,
  start_frames INTEGER,
  end_frames   INTEGER,
  PRIMARY KEY (item_id, file_id, role)
);
CREATE INDEX item_file_file ON item_file(file_id);
