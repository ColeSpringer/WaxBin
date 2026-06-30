-- WaxBin schema v11: staging/inbox import batches.
-- Each import of a staging folder into a managed library is recorded with its
-- source attribution and a tally of imported/duplicate/quarantined/errored files,
-- so an import is auditable and reviewable after the fact.

CREATE TABLE import_batch (
  id          INTEGER PRIMARY KEY,
  pid         TEXT    NOT NULL UNIQUE,
  source      TEXT    NOT NULL,                 -- staging folder / attribution
  library_id  INTEGER REFERENCES library(id) ON DELETE SET NULL,
  state       TEXT    NOT NULL,                 -- running|done|failed
  imported    INTEGER NOT NULL DEFAULT 0,
  duplicates  INTEGER NOT NULL DEFAULT 0,
  quarantined INTEGER NOT NULL DEFAULT 0,
  errored     INTEGER NOT NULL DEFAULT 0,
  bytes       INTEGER NOT NULL DEFAULT 0,       -- bytes brought in
  started_at  INTEGER NOT NULL,                 -- unix nanoseconds
  finished_at INTEGER
);
CREATE INDEX import_batch_started ON import_batch(started_at);
