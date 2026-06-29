-- WaxBin schema v7: the analyze pass's loudness and waveform outputs.
-- Both are derived data keyed by file, recomputed when the file's essence or the
-- analysis version changes (the file's analyzed_essence/analysis_version stamp
-- drives that). Stored in the catalog, not as sidecars, so analyze never writes
-- into an in-place tree. album_gain_db/album_peak are filled by the album-aware
-- aggregation that runs after the per-file pass.

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
  version      INTEGER NOT NULL,         -- peaks.Version
  bucket_count INTEGER NOT NULL,
  data         BLOB    NOT NULL,         -- packed little-endian uint16 buckets
  updated_at   INTEGER NOT NULL
);
