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
- **Library + CLI, no network daemon.** Storage is SQLite (WAL) on a local
  filesystem, with a flock-based single-writer ownership seam. The only server is an
  optional local control **socket** (`waxbin serve`), a unix socket that lets a second
  terminal mutate the catalog while one process holds the write lock. There is no HTTP
  or network listener.

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
| **Lifecycle** | `init`, `scan`, `analyze`, `watch`, `serve`, `doctor`, `jobs`, `version`, `exit-codes` |
| **Read / browse** | `query`/`ls` (incl. `--tag KEY=VALUE`, `--tag-contains`, `--tag-present`/`--tag-missing`, `--limit-mode`/`--seed`), `browse <list>`, `facet --group-by` (incl. `tag.<KEY>`, `library`), `search`, `show`, `art`, `lyrics`, `stats [--year N]`, `provenance`, `lock`/`unlock` |
| **Curation & editing** | `edit`, `entity`, `credit`, `tag`/`tag keys`, `lyrics set`, `chapters`, `art set` |
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
  queries always work). Stop the watcher (Ctrl-C) to do manual mutation, or run
  `waxbin serve` instead when a second terminal needs to mutate concurrently (see
  [Serving](#serving-multi-client)). Idle lock release is deliberately post-1.0.

### Serving (multi-client)

`waxbin serve [--socket <path>]` opens the catalog read-write (taking the write lock)
and listens on a local unix control socket (default `<db>.waxsock`, created owner-only
`0600`). While it runs, other `waxbin` commands against the same catalog no longer fail
with an ownership conflict. They **auto-detect** the running server (advertised in the
lockfile) and dispatch through it: fast mutations (`edit`, `lock`, play state,
ratings/stars, playlist membership, `user`, `merge`) are proxied to the server, and the
heavier mutating commands borrow the lock through a maintenance-mode hand-off. Read
commands always run directly. This is a local socket only, with no network or HTTP
listener. The server runs until interrupted (Ctrl-C / SIGTERM).

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

### Curation & editing

The catalog is authoritative, and a curation edit changes the catalog first: it
auto-locks the field so a later scan or enrichment pass never re-derives over your
change, and records provenance (who set the value and when).

- `waxbin edit <pid> --set field=value` edits scalar metadata such as title, artist,
  album, year, and track/disc numbers.
- `waxbin entity <pid> ...` edits a normalized entity (an artist or album name, or a
  sort name) and its identifiers (ISRC, barcode, MusicBrainz IDs).
- `waxbin credit <pid> ...` curates contributor roles such as composer and performer.
- `waxbin tag <pid> --key KEY --value V` sets a **custom tag**: a non-standard frame a
  file carries that WaxBin's typed model does not map, or one you add yourself.
  `waxbin tag <pid>` lists an item's tags, and `waxbin tag keys` lists every custom-tag
  key in the catalog with per-key item counts.
- `waxbin lyrics set`, `waxbin chapters`, and `waxbin art set` curate lyrics, book
  chapters, and cover art.

Each of these edits the catalog and offers **opt-in `--write-back`** to also mirror the
change into the backing file(s) (see below). Write-back is best-effort: a file that
cannot be written returns a typed error naming the files, while the catalog edit still
stands. A book re-anchors its identity on write-back so a later rescan resolves the same
item, and a multi-file book is written across every part.

### On-disk tag write-back (opt-in)

The catalog is always authoritative; these opt-in features mirror an edit back into
files for external players, always preserving audio essence (an essence-verified write
never alters the audio):
- The curation edits above (`edit`, `entity`, `credit`, `tag`, `lyrics set`,
  `chapters`, `art set`) take `--write-back` to embed the committed change into the
  item's file(s).
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

### Custom-tag queries

Custom tags are filterable and facetable, so they can power browse dimensions and
allow/deny rules.

- **Query filters:** `waxbin query --tag MOOD=happy` (equality), `--tag-contains
  MOOD=hap` (substring), `--tag-present MYKEY` and `--tag-missing MYKEY` (presence).
  A value may itself contain `=`, since only the first `=` splits key from value
  (`--tag DISCOGS_RELEASE=id=12345`). The same fields are available in smart-playlist
  rules as `tag.<KEY>` with the `is`, `isNot`, `isPresent`, `isMissing`, `contains`,
  `startsWith`, and `endsWith` operators. Ordered operators are rejected, since tag
  values are unordered text.
- **Facets and discovery:** `waxbin facet --group-by tag.MOOD` counts items per
  distinct value, and `waxbin tag keys` lists the keys that exist to facet on.
- **`isNot` is deny-list semantics.** `tag.X isNot V` means the item does not carry
  value V for key X. It does not mean "carries some value other than V". So an item
  tagged `MOOD=[happy, sad]` is not matched by `tag.MOOD isNot happy`, and an item with
  no MOOD tag at all is matched. That is exactly "deny when the forbidden value is
  present".
- **Case sensitivity.** Equality (`is`, `isNot`) is exact-case (BINARY). Substring
  (`contains`, `startsWith`, `endsWith`) is ASCII-case-insensitive (SQLite `LIKE`).
  Facet value buckets are case-sensitive too, so `happy` and `Happy` are distinct
  buckets; only tag *keys* are canonicalized to uppercase, never values. Folding browse
  buckets is the consumer's job. A tag key WaxBin already owns through a scalar, credit,
  or identifier surface (such as `TITLE` or `ISRC`) is reserved and rejected as a tag
  field.

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
`model/`, `store/sqlite/`, `identity/`, `decode/`, `analyze/`, `loudness/`, `peaks/`,
`fingerprint/`, `envelope/`, `query/`, `read/`, `art/`, `meta/`, `scan/`, `organize/`,
`inbox/`, `trash/`, `playback/`, `playlist/`, `podcast/`, `source/`, `enrich/`,
`audit/`, `jobs/`, `watch/`, `proxy/`, `port/`, `config/`, `pidpath/`, `waxerr/`, and
`internal/`.

## License

MIT. See [LICENSE](LICENSE).