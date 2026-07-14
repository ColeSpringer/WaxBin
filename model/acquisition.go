package model

// Acquisition is sparse item-level origin provenance. It records how and where an
// item entered the library, separate from an episode's live enclosure_url, which
// remains the download pointer retention uses later.
//
// A row exists only for an item with evidence of external origin: either an
// acquisition WaxBin performed, or the file's own SOURCE_URL/SOURCE_ID tags.
// Evidence from an event always wins over evidence from a tag. An item with neither
// has no row and reads as source:local.
//
// The older phrasing, "a locally scanned file never has an acquisition row", assumed
// origin could only be learned from an acquisition event. A scanned file carrying
// acquisition tags is evidence too, so the rule turns on the evidence rather than on
// which code path created the item.
type Acquisition struct {
	SourceType      SourceType
	SourceURL       string
	SourceID        string
	Provider        string
	ProviderVersion string
	AcquiredAt      int64 // unix nanoseconds
	OptionsJSON     string
}

// AcquisitionInput records origin provenance against an already-cataloged item.
type AcquisitionInput struct {
	SourceType      SourceType
	SourceURL       string
	SourceID        string
	Provider        string
	ProviderVersion string
	// AcquiredAt is the historical acquisition time in unix nanoseconds. Zero is the
	// sentinel for "stamp it for me": the store substitutes the current time, so a
	// zero never reaches the NOT NULL column. A caller that knows the real time (an
	// ACQUISITION_DATE tag, a download record) passes it and keeps it.
	AcquiredAt  int64
	OptionsJSON string
}
