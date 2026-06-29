-- WaxBin schema v6: sparse field provenance and user locks.
-- A row exists only when a field is not plain tag-sourced: it was edited by a
-- user, written by enrichment or organize, or locked. The absence of a row means
-- "from the tag, unlocked" (the common case), so the table stays sparse. Writers
-- (organize tag write-back, enrichment) consult locked before overwriting, so
-- curated data is protected.

CREATE TABLE field_provenance (
  item_id    INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  field      TEXT    NOT NULL,        -- canonical field name (title|artist|album|...)
  source     TEXT    NOT NULL,        -- tag|user|enrichment|organize
  locked     INTEGER NOT NULL DEFAULT 0,
  value      TEXT,                    -- the curated value, when set by a user edit
  updated_at INTEGER NOT NULL,        -- unix nanoseconds
  PRIMARY KEY (item_id, field)
);
CREATE INDEX field_provenance_locked ON field_provenance(item_id) WHERE locked = 1;
