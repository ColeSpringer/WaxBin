# Deferred work

Known gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Two sections. **Open** is work still to do. **Settled** is work deliberately not
done, kept so the same question is not re-argued from scratch.

## Open

### Music credits are one artist per track

A scanned track stores one denormalized artist string and one `artist_id`, so
`"Jay-Z feat. Alicia Keys"` becomes a single artist entity named for the whole
credit. The model for the alternative already exists: `item_contributor` is
role-tagged and supports music roles, but only a user edit (`SetCredits`) writes it,
never the scanner. `meta.SplitCredits` already splits credit strings for book
authors.

Consequence today: a joint credit gets no MusicBrainz id at all. `model.SoleMBID`
takes an id only when the file names exactly one, because stamping the first artist's
id onto the combined entity misattributes it, and every writer of an entity MBID
fills only when empty, so a wrong id is not self-correcting. That stops the
misattribution without fixing the modeling.

The fix is credit splitting at scan: one track resolving to N artist entities through
`item_contributor`, each keyed and MBID-stamped on its own. It reaches the rollups,
the artist facet, and every read surface that assumes one artist per track, so it is
its own piece of work.

### `album.mbid` is never enriched

`lookupReleaseGroup` requests `inc=artist-credits+genres`, so the music path
structurally cannot see a release id. Deciding which release of a group a local rip is
(track count, barcode, media) is a matching problem, not a projection. Consequence: an
enriched-rather-than-tagged library carries `ReleaseGroupMBID` and no `AlbumMBID`.

### Dead columns

`series.mbid` and `album.edition` have no writer. Cheaper to remove than it looks
under the pre-1.0 single-migration policy, but `series.mbid` is read (it feeds
`read.EntityInfo.MBID` for a series), so dropping it is an API change rather than a
tidy-up.

## Settled

### Enrichment durability: resolved by the on-disk write-back

Enrichment used to write the catalog and nothing else, so a retag cleared a book's
asin/isbn/publisher and a track's genre, and the enrichment marker then stopped
`enrich` refilling them. Fixed by writing what the pass filled back into the files
(`WriteEnrichmentTags` / `enrich --write-tags`, off by default), which also required
teaching the reader those tags: `applyBookFields` previously never populated
ASIN/ISBN/publisher at all, which is why the scanner cleared them regardless of how a
file was tagged.

Two things to keep in mind before touching that path. A book's asin and isbn feed
`identity.BookKey`, so writing them re-keys the item unless the pass re-anchors the
stored key from the file's post-write tags; without the re-anchor the book's pid, play
state and locks are orphaned on the next scan. And a book's publisher shares the LABEL
frame (TPUB, Vorbis and Matroska PUBLISHER) with an album's label, disambiguated by
kind in `applyBookFields` because a book consumes no album label.

Still deliberately DB-only, because the reader does not fill them: subtitle, edition,
description, and a book's mbid.

### `artist.mbid` uniqueness is unenforced

`03_entities.sql` declares a bare `mbid TEXT` with no index, so both the scan fill and
`ApplyArtistEnrichment` can leave two rows sharing one id. `entityedit.go` rejects a
duplicate on the edit path only, because relation resolution reads a single artist by
mbid. Tolerated rather than guarded: two artists sharing an mbid is exactly what
`audit duplicate_artist` reports and `waxbin merge` fixes.

### Artist identity stays name-keyed

`identity.ArtistKey` is written MBID-first and is deliberately never called;
`resolveArtist` keys on `identity.MatchKey(name)`. Release groups and albums are
MBID-first, so the asymmetry is real, but MBID-first identity does not remove
duplicate entities, it changes which ones you get: an artist tagged across half a
library forks into `mbid:x` and `name:y`, splitting on tag coverage rather than on
spelling. That is the worse failure, because partial tagging is the normal state.
The MBID is an attribute; `merge` is the reconciliation tool.

### A scan stores identifiers verbatim, an edit validates them

`waxbin edit` runs values through `model.Normalize*` (canonical UUID, GTIN check
digit); the scanner stores what the tag says. Deliberate, and stated in the
`model/identifiers.go` header: rewriting a scanned value puts the catalog out of step
with the file it claims to mirror. So `album.barcode` can read back with the spaces it
was tagged with, and a barcode an edit would reject can still arrive from a file.
Normalize before comparing.

### The projected artist MBIDs stay correlated subqueries

`itemViewCols` seeks the artist pair per row rather than joining, because
`itemArtistIDExpr` COALESCEs across track and book. Serving them from two more joins
does cut four per-row probes to two, but the extra joins cost `album_pid` its
`track_album_id` index seek, which `TestLoweredIdentityFieldPlans` catches. The
identifier projection costs about 15% on `BenchmarkQueryPageAtScale` (689 to 792 us
per 50-item page); that is the accepted price.

### `missing_mbid` reports per item

It follows `missing_art` (per-item findings capped by `Sample`, then a roll-up) rather
than `missing_replaygain` (a bare count), because the pid is what a caller acts on. It
will be the loudest check in a default run on an untagged library. That is bounded and
precedented.
