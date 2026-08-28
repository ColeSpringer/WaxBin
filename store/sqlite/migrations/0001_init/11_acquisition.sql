-- Sparse item-level origin provenance. A row exists only for an item with
-- evidence of external origin: either an acquisition WaxBin performed, or the
-- file's own SOURCE_URL/SOURCE_ID tags. A non-empty field of an event beats a
-- tag; emptiness is never evidence, so a re-record merges field by field rather
-- than clobbering. An item with neither has no row and reads as source:local. This is the historical attribution record, distinct from an
-- episode's live enclosure_url (the download pointer retention uses later).
CREATE TABLE acquisition (
  item_id          INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  source_type      TEXT    NOT NULL,               -- rss|youtube|manual
  source_url       TEXT    NOT NULL DEFAULT '',    -- the origin URL (channel/video/enclosure)
  source_id        TEXT    NOT NULL DEFAULT '',    -- provider-native id (video/channel), when any
  provider         TEXT    NOT NULL DEFAULT '',    -- provider name that acquired it
  provider_version TEXT    NOT NULL DEFAULT '',    -- provider build/version, for reproducibility
  acquired_at      INTEGER NOT NULL,               -- unix ns the item was acquired
  options_json     TEXT    NOT NULL DEFAULT ''     -- opaque provider options captured at acquisition
);
CREATE INDEX acquisition_source ON acquisition(source_type);

-- Sparse per-entity enrichment marker. A row exists once an entity has been
-- looked up, recording whether a provider matched it, the resolved MBID, and
-- when. Absence means "not yet enriched" (the iteration queue); matched=0
-- records a completed lookup that found nothing, so an unmatchable entity is
-- not retried every run (a forced re-enrich ignores the marker).
-- entity_type carries two vocabularies at once: four values name an entity the coverage
-- report counts or an album's release match, and the rest are per-pass markers keyed by
-- whatever id that pass walks. See the enrichEntity* constants for why a new pass takes
-- its own value.
CREATE TABLE entity_enrichment (
  entity_type TEXT    NOT NULL,           -- artist|release_group|book|album|lyrics|aux_art|artist_art
  entity_id   INTEGER NOT NULL,
  provider    TEXT    NOT NULL,           -- what decided it: musicbrainz, musicbrainz:edition,
                                          -- an injected provider's name, or none
  matched     INTEGER NOT NULL DEFAULT 0, -- 1 when a provider returned a usable match
  mbid        TEXT,                       -- the resolved MBID, for the passes that resolve one
  enriched_at INTEGER NOT NULL,
  PRIMARY KEY (entity_type, entity_id)
);

-- Provider response cache, keyed by a provider-scoped request key (e.g.
-- "mb:artist:<mbid>" or "mb:rg-search:<key>"), holding the raw JSON payload
-- and when it was fetched. A re-run reads the cache instead of the network, so
-- enrichment degrades gracefully offline and never re-fetches an answer it
-- already has. There is no TTL: a forced re-enrich bypasses the cache to
-- refresh.
CREATE TABLE enrichment_cache (
  cache_key  TEXT    PRIMARY KEY,
  payload    BLOB    NOT NULL,
  fetched_at INTEGER NOT NULL
);
