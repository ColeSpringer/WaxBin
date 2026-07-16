-- A subscribed feed. A feed's identity is its URL or its <podcast:guid>, never
-- its title, so a retitled show keeps its pid and episodes. A truncated feed
-- (older items dropped) never deletes the episodes it stopped listing: upserts
-- add and update, they do not prune. source_type is the acquisition provider:
-- 'rss' is an HTTP feed; 'youtube' is an injected provider (a channel/playlist
-- URL in feed_url); 'manual' is a user-curated show with no feed to sync
-- (episodes arrive via UpsertEpisode).
CREATE TABLE podcast (
  id              INTEGER PRIMARY KEY,
  pid             TEXT    NOT NULL UNIQUE,
  feed_url        TEXT    NOT NULL UNIQUE,
  identity_key    TEXT    NOT NULL UNIQUE,        -- identity.PodcastKey (guid|feed)
  source_type     TEXT    NOT NULL DEFAULT 'rss', -- rss|youtube|manual
  title           TEXT    NOT NULL,
  sort_key        TEXT    NOT NULL,
  author          TEXT    NOT NULL DEFAULT '',
  description     TEXT    NOT NULL DEFAULT '',
  link            TEXT    NOT NULL DEFAULT '',     -- show website
  language        TEXT    NOT NULL DEFAULT '',
  category        TEXT    NOT NULL DEFAULT '',     -- primary iTunes category
  explicit        INTEGER NOT NULL DEFAULT 0,
  image_url       TEXT    NOT NULL DEFAULT '',     -- current feed image URL (skip re-fetch when unchanged)
  guid            TEXT    NOT NULL DEFAULT '',     -- <podcast:guid> when published
  -- HTTP conditional-GET validators so an unchanged feed answers 304 and costs
  -- no bytes on the next sync.
  etag            TEXT    NOT NULL DEFAULT '',
  last_modified   TEXT    NOT NULL DEFAULT '',
  last_fetched_at INTEGER,                          -- unix ns of last successful sync
  -- Retention: keep the newest N downloaded episodes (0 = keep all). Enforced
  -- via the prune delete path so reclaimed space bypasses the trash.
  retention_keep  INTEGER NOT NULL DEFAULT 0,
  auth_user       TEXT    NOT NULL DEFAULT '',     -- basic-auth user (the secret table holds the password)
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX podcast_sort ON podcast(sort_key);

-- Podcast episode subtype. Shares its PK with playable_item (kind='episode'),
-- mirroring track/book. Episodes are cataloged from the feed with
-- state='remote' (known but not local) and gain a file when downloaded
-- (state='present'); retention drops the downloaded file and returns the
-- episode to 'remote' while keeping its play_state (the play_state FK is to
-- the item, which survives). The enclosure_* columns are the remote media
-- pointer that survives a download/retention cycle. pinned marks an explicitly
-- kept episode retention never reclaims, so a pinned download outlives a
-- keep-newest-N sweep.
CREATE TABLE episode (
  item_id         INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  podcast_id      INTEGER NOT NULL REFERENCES podcast(id) ON DELETE CASCADE,
  guid            TEXT    NOT NULL DEFAULT '',     -- normalized feed <guid>
  description     TEXT    NOT NULL DEFAULT '',
  link            TEXT    NOT NULL DEFAULT '',
  pub_date        INTEGER,                          -- unix ns of publication
  year            INTEGER,                          -- publication year (browse/sort)
  season          INTEGER,                          -- itunes:season
  episode_no      INTEGER,                          -- itunes:episode
  episode_type    TEXT    NOT NULL DEFAULT 'full',  -- full|trailer|bonus
  duration_ms     INTEGER,                          -- feed-declared duration
  explicit        INTEGER NOT NULL DEFAULT 0,
  pinned          INTEGER NOT NULL DEFAULT 0,
  enclosure_url   TEXT    NOT NULL DEFAULT '',
  enclosure_type  TEXT    NOT NULL DEFAULT '',
  enclosure_size  INTEGER NOT NULL DEFAULT 0,
  transcript_url  TEXT    NOT NULL DEFAULT '',      -- <podcast:transcript> src
  transcript_type TEXT    NOT NULL DEFAULT '',
  chapters_url    TEXT    NOT NULL DEFAULT '',      -- <podcast:chapters> (JSON)
  image_url       TEXT    NOT NULL DEFAULT '',      -- episode artwork URL
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX episode_podcast ON episode(podcast_id);
CREATE INDEX episode_pubdate ON episode(pub_date);

-- Stored transcript text per episode. The writer keeps transcript_fts in sync
-- inside the same transaction, so a body hit can be ranked below a title hit
-- at search time.
CREATE TABLE episode_transcript (
  item_id    INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  format     TEXT    NOT NULL DEFAULT '',           -- srt|vtt|json|text
  body       TEXT    NOT NULL,
  source_url TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
