# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## `PutScannedTrack`

The edit-transaction rename pre-pass (`renameEntitiesForEditsTx`) keeps an album's
pid and attachments when a whole release is renamed through the edit surfaces, but
the scan path can regroup the same albums outside any edit transaction: the
heuristic album key embeds the folder, so an organize or a manual folder move plus a
rescan re-keys every album that moved, and the old entities ghost with their
curation, art, and stars. The pre-pass cannot see this, because a scan resolves one
file at a time with no batch to check all-members against. Closing it needs its own
design around scan reconciliation, matching old and new chains across a scan rather
than inside one write.

## `fillEntityAuxArtTx`

The single entity-level `art` curation lock gates enrichment writes for every role
under this design: a locked entity skips front and aux fills alike, on the reading
that a locked entity's art is curated as a whole. A user who wants to lock only the
front while leaving the back open to enrichment has no way to say so. The gap runs
the other way too: `art set --role back --clear` records nothing (the lock
instruction applies to the front alone), so the vacancy reads as fillable and the
next enrichment pass can put the image back; the only defense is the whole-entity
art lock, which also blocks the front fills the user may still want. A per-role
lock is a schema and surface question of its own (a lock column or row per role,
plus `art lock`/`art roles` growing role arguments), so it is not folded into the
aux-fill change.

## `gatherArt`

Aux art is not re-fetched for an entity whose front is already settled: the fetch
pre-guards key on the front (`!ArtLocked` on the release-group pass, `!HasArt` on
the album pass), so an entity with a front cover but empty aux slots is never asked
again, and aux images land only when a pass fetches art anyway. Re-asking would
spend a rate-limited request per already-covered entity on every run for images most
providers do not have. If aux coverage matters on its own, it needs a separate
pre-guard that inspects the aux slots, and likely its own marker granularity.

## `renameArtistsForEditsTx`

The artist stage of the rename pre-pass covers track credits only. A book author is
an artist entity too, and a whole-set edit of a book's author still splits: book
references deliberately block the artist rename, since the pre-pass overlays and
re-derives track chains and cannot prove a book edit moves every reference. Renaming
an author in place needs the book edit path to participate in the pre-pass the way
track entries do.

## `adoptEntityKeyMBIDs`

With the edit path re-anchored on the entity key, no item-level edit detaches one
member from an mbid-keyed album; the member's keys are carried from the rows it
already sits on, which matches scan behavior, where the file's own MusicBrainz tags
pin the album. A user who wants one member out of an identified album has to clear
the entity mbid through `waxbin entity` first, which is the existing escape hatch.
If a direct per-member detach gesture turns out to be wanted, it would be a new edit
surface, not a change to the carryover.
