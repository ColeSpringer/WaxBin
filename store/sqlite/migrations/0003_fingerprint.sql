-- WaxBin schema v3: the fingerprint index for alt-encoding grouping.
-- The analyze pass writes one fingerprint per audio file plus a bounded set of
-- min-hash terms. Grouping finds candidate alt-encodings by shared terms within
-- a duration bucket (never a pairwise scan), then verifies with the full vector.

CREATE TABLE fingerprint (
  file_id         INTEGER PRIMARY KEY REFERENCES file(id) ON DELETE CASCADE,
  essence_hash    TEXT    NOT NULL,   -- the essence this fingerprint was computed from
  algo_version    INTEGER NOT NULL,   -- fingerprint.AlgoVersion; a bump forces re-analysis
  duration_bucket INTEGER NOT NULL,   -- coarse track-length bucket for candidate pruning
  fp              BLOB    NOT NULL     -- packed sub-fingerprint vector (verification)
);
CREATE INDEX fingerprint_bucket  ON fingerprint(duration_bucket);
CREATE INDEX fingerprint_essence ON fingerprint(essence_hash);

-- Inverted index: each min-hash term points at the files carrying it. Bounded to
-- a fixed number of terms per file so the table stays linear in the catalog size.
CREATE TABLE fingerprint_term (
  term    INTEGER NOT NULL,
  file_id INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  PRIMARY KEY (term, file_id)
);
CREATE INDEX fingerprint_term_file ON fingerprint_term(file_id);
