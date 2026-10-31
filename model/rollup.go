package model

// RollupSettle counts the entities a SettleRollups call recomputed, by kind: an album
// settles its year, the others their rollups.
type RollupSettle struct {
	Artists       int
	ReleaseGroups int
	Genres        int
	Albums        int
}

// RefreshReport counts what a RefreshRollups rewrote, each from the drift check that
// reports it, so the report and the check cannot disagree: the artists, release groups
// and genres whose rollups were not their recompute, the books whose total duration or
// ISBN key had drifted, the albums and items whose year moved, and the static playlists
// renumbered.
type RefreshReport struct {
	Artists            int
	ReleaseGroups      int
	Genres             int
	BookDurations      int
	ISBNKeys           int
	AlbumYears         int
	ReleaseYears       int
	PlaylistsCompacted int
}
