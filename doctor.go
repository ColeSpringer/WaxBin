package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// DoctorReport summarizes catalog health and detected capabilities.
type DoctorReport struct {
	DBPath        string
	SchemaVersion int
	ReadOnly      bool
	Owner         sqlite.OwnerInfo
	LibraryCount  int
	ItemCount     int
	Coverage      []FormatCoverage
}

// FormatCoverage reports, per format, whether cataloging and analysis are
// available in this build.
type FormatCoverage struct {
	Format   string
	Catalog  string // how tags/essence are read
	Analysis string // how PCM is decoded for the analyze pass
}

// Doctor reports catalog stats and capability coverage. It is read-only.
func (l *Library) Doctor(ctx context.Context) (*DoctorReport, error) {
	rep := &DoctorReport{
		DBPath:        l.opts.DBPath,
		SchemaVersion: sqlite.SchemaVersion,
		ReadOnly:      l.ReadOnly(),
		Coverage:      formatCoverage(),
	}

	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	rep.LibraryCount = len(libs)

	n, err := l.store.CountItems(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		return nil, err
	}
	rep.ItemCount = n

	// The lockfile is read without taking the lock, so even a read-only doctor
	// can report who currently owns the catalog (empty when no one holds it).
	if info, err := l.store.OwnerInfo(); err == nil {
		rep.Owner = info
	}
	return rep, nil
}

// formatCoverage reports current catalog and analysis support.
func formatCoverage() []FormatCoverage {
	const notAvailable = "not available"
	return []FormatCoverage{
		{Format: "mp3", Catalog: "pure-go (ID3v2 + essence)", Analysis: notAvailable},
		{Format: "flac", Catalog: "pure-go (Vorbis comments + essence)", Analysis: notAvailable},
		{Format: "wav", Catalog: "pure-go (filename fallback)", Analysis: notAvailable},
		{Format: "ogg/opus", Catalog: "pure-go (filename fallback)", Analysis: notAvailable},
		{Format: "aac/alac/m4a", Catalog: "pure-go (filename fallback)", Analysis: notAvailable},
	}
}
