-- WaxBin schema v16: audiobook subtype.
-- A book is a playable_item (kind='book'). It groups under a series (the album
-- abstraction for books), credits people through a role-tagged contributor
-- relation (reused by music later), and carries navigation chapters. A book is
-- backed by one file (single-file M4B) or many (one part per file); its files
-- are item_file edges, and chapters are file-relative offsets ordered into a
-- book timeline on read.

-- The book grouping above the individual title (decimal/string sequence lives on
-- the book, not the series). match_key dedups by normalized name; mbid keys it
-- directly once enrichment lands.
CREATE TABLE series (
  id        INTEGER PRIMARY KEY,
  pid       TEXT    NOT NULL UNIQUE,
  name      TEXT    NOT NULL,
  sort_key  TEXT    NOT NULL,
  match_key TEXT    NOT NULL UNIQUE,
  mbid      TEXT
);

-- Audiobook subtype. Shares its PK with playable_item, mirroring track. It keeps
-- denormalized display columns (author/narrator/series sequence) alongside FK
-- links to normalized entities (author_id -> artist, series_id -> series), so the
-- read view renders without extra joins while browse/facet use the entities.
CREATE TABLE book (
  item_id         INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  subtitle        TEXT    NOT NULL DEFAULT '',
  author          TEXT    NOT NULL DEFAULT '',   -- primary author display
  author_sort     TEXT    NOT NULL DEFAULT '',
  author_id       INTEGER REFERENCES artist(id), -- primary author entity
  narrator        TEXT    NOT NULL DEFAULT '',   -- joined narrator display
  series_id       INTEGER REFERENCES series(id) ON DELETE SET NULL,
  series_seq      TEXT    NOT NULL DEFAULT '',   -- decimal/string sequence ("1", "1.5")
  series_seq_sort TEXT    NOT NULL DEFAULT '',   -- zero-padded form for ordering
  year            INTEGER,
  publisher       TEXT    NOT NULL DEFAULT '',
  asin            TEXT    NOT NULL DEFAULT '',
  isbn            TEXT    NOT NULL DEFAULT '',
  edition         TEXT    NOT NULL DEFAULT '',
  abridged        INTEGER,                        -- NULL unknown, 0 unabridged, 1 abridged
  description     TEXT    NOT NULL DEFAULT '',
  genre           TEXT    NOT NULL DEFAULT ''     -- denormalized display, like track.genre
);
CREATE INDEX book_series ON book(series_id);
CREATE INDEX book_author ON book(author_id);

-- Role-tagged contributor relation: a person (artist entity) credited on an item
-- with a role (author|narrator|translator|editor|...). One relation reused by
-- music and audiobooks; position preserves credit order.
CREATE TABLE item_contributor (
  item_id   INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  artist_id INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
  role      TEXT    NOT NULL,                     -- author|narrator|translator|editor
  position  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, role, artist_id)
);
CREATE INDEX item_contributor_artist ON item_contributor(artist_id);

-- Navigation chapters within a book. start_ms/end_ms are offsets within file_id
-- (a single-file book has all chapters on one file; a multi-file book has each
-- part's chapters on its own file). position orders chapters within their file;
-- the read path orders by the file's item_file.position then chapter.position and
-- accumulates a book-timeline offset, so chapters need no cross-file knowledge at
-- write time. end_ms 0 means "until the next chapter or end of file".
CREATE TABLE chapter (
  id           INTEGER PRIMARY KEY,
  book_item_id INTEGER NOT NULL REFERENCES playable_item(id) ON DELETE CASCADE,
  file_id      INTEGER REFERENCES file(id) ON DELETE CASCADE,
  position     INTEGER NOT NULL,
  title        TEXT    NOT NULL DEFAULT '',
  start_ms     INTEGER NOT NULL DEFAULT 0,
  end_ms       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX chapter_book ON chapter(book_item_id);
CREATE INDEX chapter_file ON chapter(file_id);
