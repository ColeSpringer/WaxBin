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
