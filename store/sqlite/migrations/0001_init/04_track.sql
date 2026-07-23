-- Music track. Shares its PK with playable_item. Denormalized display columns
-- (the item read-view's source) sit alongside FK links to the normalized
-- entities, which browse/facet/rollups use. The WaxLabel adapter fills the tag
-- columns for every format; compilation drives Various Artists handling and
-- isrc feeds audit. MusicBrainz ids live on the entity rows (artist/album/
-- release_group .mbid); track.mbid is the recording id when known.
CREATE TABLE track (
  item_id      INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  artist       TEXT    NOT NULL DEFAULT '',
  artist_sort  TEXT    NOT NULL DEFAULT '',
  album        TEXT    NOT NULL DEFAULT '',
  album_artist TEXT    NOT NULL DEFAULT '',
  composer     TEXT    NOT NULL DEFAULT '',
  composer_sort TEXT   NOT NULL DEFAULT '',
  comment      TEXT    NOT NULL DEFAULT '',
  track_no     INTEGER,
  track_total  INTEGER,
  disc_no      INTEGER,
  disc_total   INTEGER,
  year         INTEGER,
  genre        TEXT    NOT NULL DEFAULT '',
  compilation  INTEGER NOT NULL DEFAULT 0,
  isrc         TEXT    NOT NULL DEFAULT '',
  mbid         TEXT,
  artist_id       INTEGER REFERENCES artist(id) ON DELETE SET NULL,
  album_artist_id INTEGER REFERENCES artist(id) ON DELETE SET NULL,
  album_id        INTEGER REFERENCES album(id)  ON DELETE SET NULL
);
CREATE INDEX track_artist          ON track(artist_sort);
CREATE INDEX track_album           ON track(album);
CREATE INDEX track_artist_id       ON track(artist_id);
CREATE INDEX track_album_artist_id ON track(album_artist_id);
CREATE INDEX track_album_id        ON track(album_id);
-- Recording-MBID lookup for cross-catalog ResolveRef (the strong-id rung). Partial
-- + COLLATE NOCASE keeps the index tiny (most tracks have no recording id) and
-- matches the case-insensitive lookup, since MBID case is the only cross-catalog
-- variance. Without it every strong-id resolve full-scans track.
CREATE INDEX track_mbid ON track(mbid COLLATE NOCASE) WHERE mbid IS NOT NULL;
