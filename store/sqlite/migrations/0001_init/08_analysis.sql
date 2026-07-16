-- The fingerprint index for alt-encoding grouping. The analyze pass writes one
-- fingerprint per audio file plus a bounded set of min-hash terms. Grouping
-- finds candidate alt-encodings by shared terms within a duration bucket
-- (never a pairwise scan), then verifies with the full vector.
CREATE TABLE fingerprint (
  file_id         INTEGER PRIMARY KEY REFERENCES file(id) ON DELETE CASCADE,
  essence_hash    TEXT    NOT NULL,   -- the essence this fingerprint was computed from
  algo_version    INTEGER NOT NULL,   -- fingerprint.AlgoVersion; a bump forces re-analysis
  duration_bucket INTEGER NOT NULL,   -- coarse track-length bucket for candidate pruning
  fp              BLOB    NOT NULL    -- packed sub-fingerprint vector (verification)
);
CREATE INDEX fingerprint_bucket  ON fingerprint(duration_bucket);
CREATE INDEX fingerprint_essence ON fingerprint(essence_hash);

-- Inverted index: each min-hash term points at the files carrying it. Bounded
-- to a fixed number of terms per file so the table stays linear in the catalog
-- size.
CREATE TABLE fingerprint_term (
  term    INTEGER NOT NULL,
  file_id INTEGER NOT NULL REFERENCES file(id) ON DELETE CASCADE,
  PRIMARY KEY (term, file_id)
);
CREATE INDEX fingerprint_term_file ON fingerprint_term(file_id);

-- The analyze pass's loudness and waveform outputs. Both are derived data
-- keyed by file, stamped with the essence they were computed from so a
-- measurement of superseded audio reads as absent instead of stale, and
-- recomputed when the file's essence or the analysis version changes. Stored
-- in the catalog, not as sidecars, so analyze never writes into an in-place
-- tree. album_gain_db/album_peak are filled by the album-aware aggregation
-- that runs after the per-file pass.
CREATE TABLE loudness (
  file_id         INTEGER PRIMARY KEY REFERENCES file(id) ON DELETE CASCADE,
  essence_hash    TEXT    NOT NULL,
  integrated_lufs REAL,                  -- EBU R128 integrated loudness
  track_gain_db   REAL,                  -- ReplayGain 2.0 track gain
  track_peak      REAL,                  -- linear peak amplitude
  album_gain_db   REAL,                  -- album-aware (aggregated post-pass)
  album_peak      REAL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE peaks (
  file_id      INTEGER PRIMARY KEY REFERENCES file(id) ON DELETE CASCADE,
  essence_hash TEXT    NOT NULL,
  version      INTEGER NOT NULL,         -- peaks.Version
  bucket_count INTEGER NOT NULL,
  data         BLOB    NOT NULL,         -- packed little-endian uint16 buckets
  updated_at   INTEGER NOT NULL
);
