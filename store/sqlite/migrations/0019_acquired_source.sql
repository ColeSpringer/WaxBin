-- WaxBin schema v19: acquired-media ingestion and source providers.
--
-- Three independent axes gain first-class columns. The changes are additive:
-- SQLite backfills each new NOT NULL column with its constant DEFAULT.
--
--   * source (podcast.source_type): which acquisition provider serves a show and
--     how it syncs. 'rss' is the historical behavior (an HTTP feed); 'youtube' is
--     an injected provider (a channel/playlist URL in feed_url); 'manual' is a
--     user-curated show with no feed to sync (episodes arrive via UpsertEpisode).
--   * lifecycle (episode.pinned): an explicitly kept episode retention never
--     reclaims, so a pinned download outlives a keep-newest-N sweep.
--   * library media type (library.media): a managed root carries the media it
--     holds so organize/import route an item to the type-matching library by its
--     kind. 'mixed' keeps the content-classified single-tree behavior.
--
-- The acquisition table is sparse item-level origin provenance: a row exists only
-- for an externally acquired item (a locally scanned file has none). It is the
-- historical attribution record, distinct from an episode's live enclosure_url
-- (the download pointer retention uses later).

ALTER TABLE podcast ADD COLUMN source_type TEXT NOT NULL DEFAULT 'rss';
ALTER TABLE episode ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE library ADD COLUMN media TEXT NOT NULL DEFAULT 'mixed';

CREATE TABLE acquisition (
  item_id          INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  source_type      TEXT    NOT NULL,               -- rss|youtube|manual
  source_url       TEXT    NOT NULL DEFAULT '',     -- the origin URL (channel/video/enclosure)
  source_id        TEXT    NOT NULL DEFAULT '',     -- provider-native id (video/channel), when any
  provider         TEXT    NOT NULL DEFAULT '',     -- provider name that acquired it
  provider_version TEXT    NOT NULL DEFAULT '',     -- provider build/version, for reproducibility
  acquired_at      INTEGER NOT NULL,                -- unix ns the item was acquired
  options_json     TEXT    NOT NULL DEFAULT ''      -- opaque provider options captured at acquisition
);
CREATE INDEX acquisition_source ON acquisition(source_type);
