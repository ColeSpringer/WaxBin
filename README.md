# WaxBin

WaxBin is a CGO-free Go library and `waxbin` CLI that owns the catalog database
and is the single source of truth for an audio collection. It indexes, organizes,
searches, browses, and tracks per-user state across **music, audiobooks, and
podcasts**. It runs standalone, much like `beets`, and is a clean dependency for
WaxDeck.

## Design tenets

- **No CGO, no external binaries.** Cataloging is pure Go for every format via
  [WaxLabel], and so is the analysis pass (decode, loudness, fingerprint, and
  waveforms) via [WaxFlow]. The two libraries cover the same eight containers, so
  WaxBin can decode every format it can tag-read, on every host. `fpcalc` is the
  sole remaining optional subprocess, used only for AcoustID lookups in enrichment.
- **Hard scan/analyze boundary.** Scanning is I/O-bound and never decodes PCM;
  loudness, fingerprinting, and peaks live only in a resumable analyze pass.
- **Source of truth.** Consumers read the catalog through WaxBin's canonical
  read API instead of rebuilding their own view of the filesystem.
- **Stable identities.** Every surfaced entity has an opaque sortable public id
  (ULID) that survives re-tag, move, and re-encode. Field provenance and locks
  keep enrichment and organize from ever overwriting curated data.
- **Library + CLI only.** No HTTP daemon. Storage is SQLite (WAL) on a local
  filesystem, with a flock-based single-writer ownership seam.

## Quick start

```sh
go build ./cmd/waxbin

# create the catalog and register a managed library root
./waxbin init --db ./catalog.db --root /music:managed

# index, analyze (ReplayGain/fingerprint/peaks), and organize
./waxbin scan     --db ./catalog.db
./waxbin analyze  --db ./catalog.db
./waxbin organize --db ./catalog.db --apply

# read it back
./waxbin browse newest --db ./catalog.db
./waxbin search  --db ./catalog.db "midnight"
./waxbin stats   --db ./catalog.db
```

Roots are declared as `path[:mode[:media[:profile]]]`, for example
`/music:managed:music`, `/audiobooks:managed:audiobook`, `/rips:in-place`.
Config resolves with **flag > env (`WAXBIN_*`) > JSON > default** precedence.
Every data command supports `--json` (with a `schemaVersion`) and returns stable
exit codes (`waxbin exit-codes`).

## CLI reference

| Area | Commands |
| --- | --- |
| **Lifecycle** | `init`, `scan`, `analyze`, `watch`, `doctor`, `jobs`, `version`, `exit-codes` |
| **Read / browse** | `query`/`ls`, `browse <list>`, `facet --group-by`, `search`, `show`, `art`, `lyrics`, `stats [--year N]`, `provenance`, `lock`/`unlock` |
| **Ingest / organize** | `inbox`, `import`, `organize`, `profiles` |
| **Deletion / repair** | `trash`, `rm [--permanent]`, `merge`, `audit`, `upgrade` |
| **Portability** | `backup`, `restore`, `export`, `manifest`, `rebuild` |
| **Playlists / podcasts** | `playlist`, `smartplaylist`, `podcast`, `opml` |
| **Enrichment** | `enrich` (MusicBrainz + Cover Art Archive; optional AcoustID) |
| **Maintenance** | `db verify [--fix]`, `db vacuum [--integrity]`, `db migrate`, `user`, `state` |

### Watching for changes

- `waxbin scan` is incremental: an unchanged file (same size + mtime) is skipped
  without re-hashing or re-parsing, and a file deleted on disk is reconciled to
  `missing`, behind a survival gate that refuses to act on a transiently
  unavailable root, so a momentary mount loss cannot mark a whole library missing.
  `scan --force` (alias `--full`) re-hashes and re-parses everything. A deliberate
  large deletion (more than half a library) is held back by the survival gate; run
  `scan --reconcile-deletions` to reconcile it once you have confirmed the files are
  really gone.
- `waxbin watch` keeps the catalog in sync on a schedule (and, with `--live`, on
  filesystem events). Scheduled rescans are the primary mechanism because
  filesystem events are unreliable on WSL2, NFS, SMB, and bind mounts; a periodic
  full-content rescan (`--full-interval`) catches same-size/mtime-preserved edits
  the fast-path misses.
- **`watch` is a foreground mode.** A read-write WaxBin holds an exclusive advisory
  lock on the catalog for its whole lifetime, so while `watch` runs, every other
  *mutating* command in another terminal (`organize`, `analyze`, `enrich`,
  `import`, `scan --force`) is refused with an ownership conflict (read-only
  queries always work). Stop the watcher (Ctrl-C) to do manual mutation. Idle lock
  release and a socket proxy are deliberately post-1.0.

### Quality, repair, and maintenance

- `waxbin audit` reports quality and integrity problems: duplicate/split entities,
  inconsistent metadata, missing art/ReplayGain, unportable filenames, orphaned
  sidecars, case-insensitive path conflicts, invalid feeds, and derived-data
  drift. `--integrity` adds an on-disk bitrot (content-hash) and corrupt-audio
  pass. It reports only; it never deletes.
- `waxbin merge <type> <survivor-pid> <loser-pid>...` collapses duplicate
  artists / release-groups / albums / genres onto one survivor, re-pointing
  children (so play state and provenance ride along) and recomputing rollups.
- `waxbin upgrade` groups alt encodings of the same recording (by fingerprint),
  ranks each group by quality, and marks the keeper.
- `waxbin db verify --fix` repairs derived-data drift (FTS, rollups, sort keys)
  and reclaims orphaned art. `waxbin db vacuum` GCs and compacts the database.
- `waxbin stats --year 2025` prints a per-user listening year-in-review.

### On-disk tag write-back (opt-in)

The catalog is always authoritative; these opt-in features mirror it back into files
for external players, always preserving audio essence (an essence-verified write
never alters the audio):
- `waxbin analyze --write-replaygain` (or `write_replaygain_tags` in config) writes
  computed track and album ReplayGain into files after album aggregation
  (`REPLAYGAIN_*`, or Opus `R128_*`).
- An organize profile with `tag_write` corrects `albumArtist` (literal
  `Various Artists` for compilations) and disc/track numbering on disk as it moves
  files, skipping locked fields and re-tagging before the move so a failure aborts
  cleanly.
- `stamp_item_pid` additionally stamps a `WAXBIN_ITEM_PID` tag during organize, so
  `rebuild` can restore original item identities from tags (essence-first: adopted
  only when unambiguous, minted fresh on any conflict). A full DB backup remains the
  real disaster-recovery artifact.

## Library API

```go
lib, err := waxbin.Open(ctx, waxbin.Options{
    DBPath: "catalog.db",
    Roots:  []config.Root{{Path: "/music", Mode: model.ModeManaged}},
})
defer lib.Close()

if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil { /* ... */ }
page, err := lib.Browse(ctx, read.ListNewest, read.BrowseOptions{Limit: 50})
```

The stable facade lives in the root package; implementation subsystems live under
`model/`, `store/sqlite/`, `identity/`, `decode/`, `analyze/`, `query/`, `read/`,
`art/`, `meta/`, `scan/`, `organize/`, `inbox/`, `trash/`, `podcast/`, `enrich/`,
`audit/`, `jobs/`, `pidpath/`, and `internal/`.

## License

MIT. See [LICENSE](LICENSE).