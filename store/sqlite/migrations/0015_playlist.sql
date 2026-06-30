-- WaxBin schema v15: static and smart playlists.
-- A static playlist stores an explicit ordered item list (playlist_item). A smart
-- playlist stores a versioned query rule (envelope JSON) and is evaluated on read
-- through the shared query engine, so it carries no playlist_item rows. Playlists
-- are owned by a user and carry a visibility; deleting the owner or a listed item
-- cascades.

CREATE TABLE playlist (
  id            INTEGER PRIMARY KEY,
  pid           TEXT    NOT NULL UNIQUE,
  name          TEXT    NOT NULL,
  owner_user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  kind          TEXT    NOT NULL,                    -- static|smart
  visibility    TEXT    NOT NULL DEFAULT 'private',  -- private|shared
  rule          TEXT,                                -- smart: envelope-wrapped query rule
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX playlist_owner ON playlist(owner_user_id);

CREATE TABLE playlist_item (
  playlist_id INTEGER NOT NULL REFERENCES playlist(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  item_id     INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  PRIMARY KEY (playlist_id, position)
);
CREATE INDEX playlist_item_item ON playlist_item(item_id);
