# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## `reconcileAlbumRekeyTx`

The scan-side reconciliation that carries an album's pid and attachments onto the
row a moved folder re-keys it to only fires when the old and new rows still share
one release group, so a title or artist retag stays a ghost: the retag changes the
release-group key too, and with the two rows sitting under different groups there is
nothing left to tie the drained row to its replacement. Requiring a shared release
group is the guard that keeps the reconciliation from folding together two albums
that only happen to land under the same key by coincidence, so it stays even though
it leaves this case open. Closing it needs a corroborating signal beyond the release
group, most likely the relink's prior path or the organize journal, neither of which
the reconciliation consults today.

## `SetItemCredits`

The single-entry pre-pass batch added to `SetItemCredits` renames an artist or book
author in place when the edit covers that item's only reference, but a
multi-reference rename, the same artist named across several items in one credits
call, still splits: the pre-pass proves coverage from a batch of edit entries
assembled ahead of the rename, and a per-item credits surface has no way to gather
every reference it is about to touch into one batch before it runs. A batch credits
surface, alongside the existing batch field edits, would close it.

The two surfaces also disagree about what a credit naming several people means for
identity. `SetItemCredits(RoleAuthor, ["A", "B"])` leaves the old author where it is
and splits, since one entity becoming two names leaves no successor to carry the pid,
curation, and star; `EditItemField("author", "A & B")` still renames that same entity
onto "A", because the book credit splitter runs after the overlay and the pre-pass
takes the first name as the target. Two spellings of one intent, two identity
outcomes. Settling it means picking which rule both paths follow and moving the other
one to it, which is a behaviour decision rather than a repair.

## `enrichAlbumRelease`

The auxiliary-art backfill walks release groups with an empty slot, so an album
whose front cover was already settled before it matched its MusicBrainz release
through the album-level pass never gets an aux ask of its own: the RG-only queue
never sees it, since the front-keyed gate on the release match skips fetching art
at all once an album already resolves a front, aux roles included. The sliver is
narrow, an album release-matched after its front was already filled, and closing it
needs the album pass to make its own aux ask rather than counting on the
release-group queue to catch it.

## `EditEntityFields`

Clearing an album's or a release group's mbid re-keys the chain in the catalog and
stops there: the member files still carry MUSICBRAINZ_ALBUMID and
MUSICBRAINZ_RELEASEGROUPID, so the next scan that re-resolves one of them computes
the mbid key again, forks a fresh identified album, and leaves the re-keyed row to
drain and ghost. The whole-album analogue of `detach --write-back`, one strip fanned
across every member on the clear, was deliberately not built. writeBackDetach shows
the strip itself is expressible through the shared write-back engine, but the entity
fan-out otherwise carries only values a rescan round-trips without moving the entity,
while a strip decides what the next scan resolves, across every file of an album
rather than one, and a partial failure leaves the un-stripped members to fork the
identity straight back. That wants a gesture of its own with its own refusal
reporting, so the clear stays DB-only for now and stripping the tags externally is
what makes it durable.
