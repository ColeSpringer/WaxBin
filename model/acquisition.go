package model

// Acquisition is sparse item-level origin provenance: it exists only for an
// externally acquired item (a locally scanned file has none). It records how and
// where an item entered the library, separate from an episode's live enclosure_url,
// which remains the download pointer retention uses later.
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
// AcquiredAt is stamped by the store when zero.
type AcquisitionInput struct {
	SourceType      SourceType
	SourceURL       string
	SourceID        string
	Provider        string
	ProviderVersion string
	OptionsJSON     string
}
