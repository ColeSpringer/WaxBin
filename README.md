# WaxBin

WaxBin is a CGO-free Go library and `waxbin` CLI that owns the catalog database
for an audio collection. It indexes files, records stable identities, searches
the catalog, and can organize managed libraries on disk. It can be used
standalone, much like `beets`, or embedded by WaxDeck.

> **Status:** early development. The current build covers the core loop:
> `scan -> store -> query -> organize -> read back` on local audio files. See
> [CHANGELOG.md](CHANGELOG.md) for release notes.

## Design tenets

- **No CGO.** Pure-Go cataloging for every format. `ffmpeg`/`fpcalc` are
  optional subprocesses used only by the (separate) analysis pass.
- **Hard scan/analyze boundary.** Scanning is I/O-bound and never decodes PCM.
- **Source of truth.** Consumers read the catalog through WaxBin instead of
  rebuilding their own view of the filesystem.
- **Library + CLI only.** No HTTP daemon. Storage is SQLite on a local
  filesystem.

## Quick start

```sh
go build ./cmd/waxbin

# create the catalog and register a managed library root
./waxbin init --db ./catalog.db --root /music:managed

# scan, query, organize, read back
./waxbin scan --db ./catalog.db
./waxbin query --db ./catalog.db --title "*"
./waxbin organize --db ./catalog.db --profile waxbin-native --apply
./waxbin show --db ./catalog.db <pid>
```

Every data command supports `--json` and returns stable exit codes
(`waxbin exit-codes`).

## License

MIT - see [LICENSE](LICENSE).
