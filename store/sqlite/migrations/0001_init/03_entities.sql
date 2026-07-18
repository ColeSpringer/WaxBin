-- Artist entity. Artists are resolved by normalized match_key unless stronger
-- external identifiers are available. Audiobook credits (author, narrator)
-- point at the same rows through item_contributor.
CREATE TABLE artist (
  id        INTEGER PRIMARY KEY,
  pid       TEXT    NOT NULL UNIQUE,
  name      TEXT    NOT NULL,            -- canonical display casing
  sort_key  TEXT    NOT NULL,            -- WaxBin-generated, BINARY-sortable
  match_key TEXT    NOT NULL UNIQUE,     -- normalized dedup key
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
-- src_id cascades ride the primary key; dst_id needs its own index so orphan
-- GC and merges, which delete artists in batches, do not rescan the relation
-- table once per swept artist.
CREATE INDEX artist_relation_dst ON artist_relation(dst_id);

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
  barcode          TEXT,                 -- release barcode (UPC/EAN)
  label            TEXT,                 -- record label
  catalog_number   TEXT,                 -- label catalog number
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

-- Role-tagged contributor relation: a person (artist entity) credited on an
-- item with a role, shared by music and audiobooks; position preserves credit
-- order.
CREATE TABLE item_contributor (
  item_id   INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
  role      TEXT    NOT NULL,                     -- author|narrator|translator|editor
  position  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, role, artist_id)
);
CREATE INDEX item_contributor_artist ON item_contributor(artist_id);
