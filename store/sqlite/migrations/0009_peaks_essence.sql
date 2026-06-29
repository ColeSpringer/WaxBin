-- WaxBin schema v9: stamp waveforms with the essence they were computed from.
-- This mirrors the loudness table, so a waveform measured from superseded audio
-- reads as absent instead of stale. Existing rows get NULL and are treated as
-- stale until re-analyzed.

ALTER TABLE peaks ADD COLUMN essence_hash TEXT;
