package model

// PortableRef is a catalog-independent identity descriptor for one playable item,
// used to recognize the same item across two independent catalogs (the embedder
// runs one catalog per user, so a "share" is a copy, and a playlist of local PIDs is
// meaningless in another catalog). It carries the progressively fuzzier anchors the
// resolve ladder walks: Essence is the exact-rip audio hash; the strong ids (recording
// MBID for a track, release MBID/ASIN/ISBN for a book) are exact external identifiers;
// Fingerprint matches a different encoding of the same recording; and the descriptive
// fields catch a different rip that carries neither an id nor a comparable fingerprint.
// Kind selects the strong-id/descriptive strategy and lets the host route the copy.
//
// It carries no struct tags, in keeping with the rest of model. Wire encoding is the
// host's job: it round-trips the Go struct as is, or maps it to its own DTO whose json
// tags live in the host rather than here.
type PortableRef struct {
	Kind            Kind   // track | book | episode
	Essence         string // exact-rip audio-essence hash
	Fingerprint     []byte // packed acoustic fingerprint (omitted for virtual/CUE tracks)
	FingerprintAlgo int    // fingerprint algorithm: 1 = pure-Go, 100 = Chromaprint
	MBID            string // recording MBID (track) or release MBID (book)
	ASIN            string // audiobook
	ISBN            string // audiobook
	Artist          string // track artist, or book author (the view COALESCE value)
	Title           string
	Album           string // track album, or book series (the view COALESCE value)
	DurationMS      int64
}

// MatchRung names which rung of the resolve ladder produced a match, so the host can
// communicate confidence (an essence match is exact bytes; a descriptive match is a
// fuzzy metadata guess). The rungs are ordered from most to least confident.
type MatchRung string

const (
	MatchNone        MatchRung = "none"        // no local item matched
	MatchEssence     MatchRung = "essence"     // identical audio bytes
	MatchStrongID    MatchRung = "strongId"    // exact external id: recording/release MBID, ASIN, ISBN
	MatchFingerprint MatchRung = "fingerprint" // same recording, different encoding
	MatchDescriptive MatchRung = "descriptive" // fuzzy metadata (artist/title/album/series/duration)
)

// RefResolution pairs a PortableRef with the local item it resolved to, and the rung
// that matched. PID is empty exactly when Rung == MatchNone. It is the per-entry result
// of resolving a batch (a playlist), preserving the input order so the host can rebuild
// a local playlist and report which entries were missing.
type RefResolution struct {
	Ref  PortableRef
	PID  PID // empty when Rung == MatchNone
	Rung MatchRung
}
