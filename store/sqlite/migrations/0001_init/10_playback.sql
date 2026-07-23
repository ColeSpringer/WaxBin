-- Multi-user playback state, history, and queue. A default user is seeded in
-- Go after migration because pids are ULIDs, not expressible in SQL.
-- play_state holds the per-user resume/played/counts/rating/star; sessions
-- feed stats; the queue and bookmarks are per user. Deleting an item or user
-- cascades its playback rows.
CREATE TABLE user (
  id         INTEGER PRIMARY KEY,
  pid        TEXT    NOT NULL UNIQUE,
  name       TEXT    NOT NULL UNIQUE,
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE play_state (
  user_id        INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  item_id        INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  position_ms    INTEGER NOT NULL DEFAULT 0,  -- resume position
  played         INTEGER NOT NULL DEFAULT 0,  -- played at least once
  finished       INTEGER NOT NULL DEFAULT 0,  -- played to completion
  play_count     INTEGER NOT NULL DEFAULT 0,
  rating         INTEGER,                     -- 0..100, NULL = unset
  starred_at     INTEGER,                     -- NULL = not starred
  last_played_at INTEGER,
  -- Per-field change stamps for star and rating, bumped only when the stored
  -- value actually changes (a clear included), so a sync adapter can order a
  -- local change against a remote one. NULL = never changed. No index: a replay
  -- guard compares against a row it already holds.
  rating_changed_at  INTEGER,
  starred_changed_at INTEGER,
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (user_id, item_id)
);
CREATE INDEX play_state_item    ON play_state(item_id);
CREATE INDEX play_state_recent  ON play_state(user_id, last_played_at);
CREATE INDEX play_state_starred ON play_state(user_id, starred_at);
CREATE INDEX play_state_played  ON play_state(user_id, play_count);

CREATE TABLE bookmark (
  id          INTEGER PRIMARY KEY,
  pid         TEXT    NOT NULL UNIQUE,
  user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  item_id     INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  position_ms INTEGER NOT NULL,
  label       TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE INDEX bookmark_user_item ON bookmark(user_id, item_id);
-- The user_item index leads with user_id, so the item-delete cascade cannot
-- use it; bulk item deletes (retention, trash) check this FK once per row.
CREATE INDEX bookmark_item      ON bookmark(item_id);

CREATE TABLE play_queue (
  user_id  INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  item_id  INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, position)
);
-- The primary key leads with user_id, so the item-delete cascade needs its own
-- index (same reasoning as bookmark_item above).
CREATE INDEX play_queue_item ON play_queue(item_id);

CREATE TABLE play_session (
  id         INTEGER PRIMARY KEY,
  pid        TEXT    NOT NULL UNIQUE,
  user_id    INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  item_id    INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  started_at INTEGER NOT NULL,
  ended_at   INTEGER,
  ms_played  INTEGER NOT NULL DEFAULT 0,
  client     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX play_session_user ON play_session(user_id, started_at);
CREATE INDEX play_session_item ON play_session(item_id);

-- Static and smart playlists. A static playlist stores an explicit ordered
-- item list (playlist_item). A smart playlist stores a versioned query rule
-- (envelope JSON) and is evaluated on read through the shared query engine, so
-- it carries no playlist_item rows. Playlists are owned by a user and carry a
-- visibility; deleting the owner or a listed item cascades.
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
