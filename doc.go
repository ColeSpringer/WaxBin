// Package waxbin is the public facade for the WaxBin audio library engine.
//
// WaxBin is a CGO-free Go library and waxbin CLI that owns the catalog database
// for an audio collection. It indexes files, records stable identities,
// searches the catalog, and can organize managed libraries on disk. It can be
// used standalone, much like beets, or embedded by WaxDeck.
//
// # Boundaries
//
// Cataloging is pure Go. Scanning reads tags and decoder-independent identity
// data without decoding PCM; PCM work belongs to the separate analysis pass,
// which is itself pure Go: decode, loudness, fingerprint, and waveforms all run
// through WaxFlow with no CGO and no external binaries. The read side is owned by
// WaxBin so consumers share one catalog view.
//
// # Layout
//
// The stable facade lives in this root package. Implementation subsystems live
// in their own packages: model (domain types + repository interfaces), query
// (the shared selection engine), identity (entity identity + essence hashing),
// store/sqlite (the SQLite DataStore, write coordinator, and flock ownership),
// read (facet/browse/search/pagination), art (CAS + thumbnails), decode (pure-Go
// PCM decoding via WaxFlow) and analyze (the PCM analysis pass), scan, organize,
// inbox, trash, playback, playlist, podcast, source (the acquisition port),
// enrich (metadata brain), audit (quality/repair), jobs, meta, config, and
// pidpath (item PID -> file location, cached off the change feed, for a consumer
// that serves audio by PID); the CLI lives under cmd/waxbin.
//
// The engine covers the full lifecycle: scan, analyze, organize, read/browse,
// playback state, podcasts, audiobooks, enrichment, and audit/quality/repair, all
// on the same package boundaries. The merge primitive and the maintenance commands
// (db verify/vacuum/migrate) close the loop.
package waxbin
