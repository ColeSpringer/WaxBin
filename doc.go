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
// data without decoding PCM; PCM work belongs to the separate analysis pass.
// The read side is owned by WaxBin so consumers share one catalog view.
//
// # Layout
//
// The stable facade lives in this root package. Implementation subsystems live
// in their own packages: model (domain types + repository interfaces), query
// (the shared selection engine), identity (entity identity + essence hashing),
// store/sqlite (the SQLite DataStore, write coordinator, and flock ownership),
// scan, organize, jobs, meta, and config; the CLI lives under cmd/waxbin.
//
// The current build covers the core local-file loop: scan -> store -> query ->
// organize -> read back. Analysis, enrichment, podcasts, and audiobooks build on
// the same package boundaries.
package waxbin
