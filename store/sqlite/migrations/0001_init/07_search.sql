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
-- in sync inside the same write transaction that mutates the item. Each column
-- holds its NFC text followed by the alternate forms of its words (searchtext.go),
-- and the mark categories keep an Indic or Thai word whole.
CREATE VIRTUAL TABLE search_fts USING fts5(
  kind UNINDEXED, title, subtitle, artist, album, extra, credits,
  tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

-- Transcript text lives in its own FTS so a metadata hit can outrank a body hit. The
-- body is indexed with the script folds of the search columns (searchtext.go).
CREATE VIRTUAL TABLE transcript_fts USING fts5(
  episode_id UNINDEXED, body,
  tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");
