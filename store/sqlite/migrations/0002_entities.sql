-- WaxBin schema v2: the load-bearing read-side model.
-- Normalized entities (artist, release_group, album, genre) sit alongside the
-- denormalized text columns on `track` (which the item read-view still uses).
-- The writer resolves and links these entities during scan persistence; their
-- counts/durations feed the maintained rollups, and metadata feeds the FTS.

-- Artist entity. Artists are resolved by normalized match_key unless stronger
-- external identifiers are available.
CREATE TABLE artist (
  id        INTEGER PRIMARY KEY,
  pid       TEXT    NOT NULL UNIQUE,
  name      TEXT    NOT NULL,            -- canonical display casing
  sort_key  TEXT    NOT NULL,           -- WaxBin-generated, BINARY-sortable
  match_key TEXT    NOT NULL UNIQUE,    -- normalized dedup key
  mbid      TEXT
);
CREATE INDEX artist_sort ON artist(sort_key);

-- Alternate names for an artist (is_primary marks the canonical display name).
CREATE TABLE artist_alias (
  id         INTEGER PRIMARY KEY,
  artist_id  INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  sort_key   TEXT    NOT NULL,
  is_primary INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX artist_alias_artist ON artist_alias(artist_id);
CREATE UNIQUE INDEX artist_alias_unique ON artist_alias(artist_id, name);

-- Directed artist relations (member_of | aka | similar), filled by enrichment.
CREATE TABLE artist_relation (
  src_id INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
  dst_id INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
  kind   TEXT    NOT NULL,
  PRIMARY KEY (src_id, dst_id, kind)
);

-- The "album" abstraction for browse: groups the editions/releases of one work.
CREATE TABLE release_group (
  id                INTEGER PRIMARY KEY,
  pid               TEXT    NOT NULL UNIQUE,
  title             TEXT    NOT NULL,
  sort_key          TEXT    NOT NULL,
  primary_artist_id INTEGER REFERENCES artist(id) ON DELETE SET NULL,
  mbid              TEXT,
  type              TEXT,                 -- album|ep|single|compilation|audiobook
  match_key         TEXT    NOT NULL UNIQUE
);
CREATE INDEX release_group_sort   ON release_group(sort_key);
CREATE INDEX release_group_artist ON release_group(primary_artist_id);

-- A specific release/edition under a release_group.
CREATE TABLE album (
  id               INTEGER PRIMARY KEY,
  pid              TEXT    NOT NULL UNIQUE,
  release_group_id INTEGER REFERENCES release_group(id) ON DELETE SET NULL,
  title            TEXT    NOT NULL,
  sort_key         TEXT    NOT NULL,
  year             INTEGER,
  disc_total       INTEGER,
  mbid             TEXT,
  edition          TEXT,
  match_key        TEXT    NOT NULL UNIQUE
);
CREATE INDEX album_rg   ON album(release_group_id);
CREATE INDEX album_sort ON album(sort_key);

-- Normalized genre/mood entity. One table serves both facets, with uniqueness
-- scoped by facet and match_key rather than by display name.
CREATE TABLE genre (
  id        INTEGER PRIMARY KEY,
  pid       TEXT    NOT NULL UNIQUE,
  facet     TEXT    NOT NULL DEFAULT 'genre',  -- genre|mood
  name      TEXT    NOT NULL,                  -- canonical display, e.g. "Hip-Hop"
  match_key TEXT    NOT NULL,                  -- normalized, e.g. "hip hop"
  sort_key  TEXT    NOT NULL
);
CREATE UNIQUE INDEX genre_match ON genre(facet, match_key);
CREATE INDEX genre_sort ON genre(sort_key);

CREATE TABLE item_genre (
  item_id  INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  genre_id INTEGER NOT NULL REFERENCES genre(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, genre_id)
);
CREATE INDEX item_genre_genre ON item_genre(genre_id);

-- Entity links on the music subtype (denormalized text columns stay for reads).
ALTER TABLE track ADD COLUMN artist_id       INTEGER REFERENCES artist(id) ON DELETE SET NULL;
ALTER TABLE track ADD COLUMN album_artist_id INTEGER REFERENCES artist(id) ON DELETE SET NULL;
ALTER TABLE track ADD COLUMN album_id        INTEGER REFERENCES album(id) ON DELETE SET NULL;
CREATE INDEX track_artist_id ON track(artist_id);
CREATE INDEX track_album_id  ON track(album_id);

-- Maintained rollups: catalog-structural counts/durations only. RefreshRollups
-- recomputes them in bulk from the base tables. Play-derived lists are never
-- rolled up.
CREATE TABLE artist_rollup (
  artist_id           INTEGER PRIMARY KEY REFERENCES artist(id) ON DELETE CASCADE,
  release_group_count INTEGER NOT NULL DEFAULT 0,
  track_count         INTEGER NOT NULL DEFAULT 0,
  total_duration_ms   INTEGER NOT NULL DEFAULT 0,
  updated_at          INTEGER NOT NULL
);
CREATE TABLE release_group_rollup (
  release_group_id  INTEGER PRIMARY KEY REFERENCES release_group(id) ON DELETE CASCADE,
  track_count       INTEGER NOT NULL DEFAULT 0,
  total_duration_ms INTEGER NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL
);
CREATE TABLE genre_rollup (
  genre_id          INTEGER PRIMARY KEY REFERENCES genre(id) ON DELETE CASCADE,
  track_count       INTEGER NOT NULL DEFAULT 0,
  total_duration_ms INTEGER NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL
);

-- Writer-maintained metadata FTS (no triggers): rowid == playable_item.id, kept
-- in sync inside the same write transaction that mutates the item.
CREATE VIRTUAL TABLE search_fts USING fts5(
  kind, title, subtitle, artist, album, extra,
  tokenize = 'unicode61 remove_diacritics 2');

-- Transcript text lives in its own FTS so a metadata hit can outrank a body hit.
-- The table is present before transcript production so the schema contract is
-- stable.
CREATE VIRTUAL TABLE transcript_fts USING fts5(
  episode_id UNINDEXED, body,
  tokenize = 'unicode61 remove_diacritics 2');
