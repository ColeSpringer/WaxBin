-- WaxBin schema v23: orphan-entity GC grace tracking.
-- orphan_candidate records when an entity (artist/release_group/album/genre/series)
-- was first observed childless, so the manual GC only sweeps an entity that has
-- stayed orphaned past a grace window (a two-run / time-based confirmation). This is
-- the safety backstop to the scanner's survival gate: a transient reconciliation
-- blip cannot immediately destroy MusicBrainz enrichment or operator merges.
CREATE TABLE orphan_candidate (
  entity_type TEXT    NOT NULL,          -- artist | release_group | album | genre | series
  entity_id   INTEGER NOT NULL,
  first_seen  INTEGER NOT NULL,          -- unix ns first observed orphaned
  PRIMARY KEY (entity_type, entity_id)
);
