-- WaxBin schema v5: music-complete track columns.
-- The WaxLabel adapter reads these for every format; they sit alongside the
-- existing denormalized columns and feed display, interop (compilation drives
-- Various Artists), and audit (isrc). MusicBrainz ids populate the entity rows
-- (artist/album/release_group .mbid), not the track, so they are not added here.

ALTER TABLE track ADD COLUMN composer    TEXT    NOT NULL DEFAULT '';
ALTER TABLE track ADD COLUMN comment     TEXT    NOT NULL DEFAULT '';
ALTER TABLE track ADD COLUMN track_total INTEGER;
ALTER TABLE track ADD COLUMN disc_total  INTEGER;
ALTER TABLE track ADD COLUMN compilation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE track ADD COLUMN isrc        TEXT    NOT NULL DEFAULT '';
