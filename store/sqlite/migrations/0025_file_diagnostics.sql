-- WaxBin schema v25: persist per-file diagnostics, and record which files have had
-- them derived.
--
-- origin is the writer that produced the row (scan | organize | replaygain), not the
-- phase it happened in. It is part of the primary key, so each writer replaces only
-- its own rows, and cross-writer isolation is a property of the schema rather than of
-- a delete predicate.
--
-- This also restates an invariant v19 asserted too narrowly. 0019 said "a locally
-- scanned file never has an acquisition row", which assumed origin can only be
-- learned from an acquisition event. It can also be learned from the file's own
-- SOURCE_URL/SOURCE_ID tags. The rule is about evidence, not about which code path
-- created the item:
--
--   A row exists only for an item with evidence of external origin: either an
--   acquisition WaxBin performed, or the file's own SOURCE_URL/SOURCE_ID tags.
--   Evidence from an event always wins over evidence from a tag. An item with
--   neither has no row and reads as source:local.
--
-- (0019 is applied and immutable, so the restatement lives here.)
CREATE TABLE file_diagnostic (
  file_id  INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  origin   TEXT    NOT NULL,            -- scan | organize | replaygain
  code     TEXT    NOT NULL,
  severity TEXT    NOT NULL,            -- info | warn | error
  tag_key  TEXT    NOT NULL DEFAULT '',
  detail   TEXT    NOT NULL DEFAULT '',
  seen_at  INTEGER NOT NULL,
  PRIMARY KEY (file_id, origin, code, tag_key)
);
CREATE INDEX file_diagnostic_code ON file_diagnostic(code);

-- diag_version records the diagnostic rule-set version a file was last derived
-- under. Only a full-path scan stamps it.
--
-- DEFAULT 0 is safe here because it states something true: 0 means never derived,
-- which is the case for every pre-existing row, and the audit reports that coverage
-- rather than letting an absence of rows read as a clean library. v22 is the warning
-- against the alternative. Its chapter.source DEFAULT 'embedded' asserted something
-- false about every pre-existing row, and v24 had to repair the result.
--
-- A version mismatch forces nothing. The scan fast-path's escape skips more than the
-- tag re-read: it also skips content hashing and a per-file write transaction, so
-- forcing a re-derive would amount to `scan --force`, meaning hours of I/O on a large
-- library for advisory data, and no warning for anyone running `watch`.
ALTER TABLE file ADD COLUMN diag_version INTEGER NOT NULL DEFAULT 0;
