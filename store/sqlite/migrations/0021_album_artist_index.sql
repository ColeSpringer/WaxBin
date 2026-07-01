-- WaxBin schema v21: index track(album_artist_id).
-- Schema v2 indexed track(artist_id) and track(album_id) but not album_artist_id.
-- Enrichment's "does this artist back any item" test
-- (track.artist_id = a.id OR track.album_artist_id = a.id) then has to scan every
-- track for the album-artist half. That test runs once per artist in
-- ArtistsNeedingEnrichment and in CountEntitiesNeedingEnrichment (which doctor calls),
-- so index the column and let the album-artist half use an index like the artist_id
-- half already does.
CREATE INDEX IF NOT EXISTS track_album_artist_id ON track(album_artist_id);
