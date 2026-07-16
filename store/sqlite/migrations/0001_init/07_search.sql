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
CREATE VIRTUAL TABLE transcript_fts USING fts5(
  episode_id UNINDEXED, body,
  tokenize = 'unicode61 remove_diacritics 2');
