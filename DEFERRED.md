# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## Artist rename over a contributor role

`RenameEntity` at the artist rung covers the references `EditItemsFields` already applies
to: `track.artist_id`, `track.album_artist_id`, and `book.author_id`, plus the
`item_contributor` rows for the artist and author roles those fields write. Every other
role (producer, composer, narrator, translator, editor) is refused with `CodeConflict`
naming the item, so the rename never moves an artist's curation out from under a credit
row still spelling the old name.

The pre-pass is already ready for it: `renameArtistsForEditsTx` takes members, books and
credits in one call and the edit path just passes nil. What is missing is the apply after
it. Non-artist roles live on `SetItemCreditsBatch`, and the two surfaces have never shared
a transaction, so getting provenance rows, lock validation, rollups and deltas right for
both at once is new code rather than reuse. It was left out deliberately: the common case
does not need it, and the refusal is loud, so nothing is silently wrong in the meantime.

## Book identity forks on an enrichment-derived ASIN

The entity sibling of this was closed by resolve-time mbid adoption; the item one is open.
`BookEnrichment` fills `book.asin` and `book.isbn` without moving the item's identity key,
`identity.BookKey` keys on those identifiers, and the item resolver looks a book up by
`playable_item.identity_key` alone. So a part file that later arrives already tagged with
an enrichment-derived ASIN computes a key that matches nothing and forks a second book
item, exactly the shape `resolveReleaseGroup` had.

Lower severity than the entity one, because enrichment write-back, when it is enabled,
stamps every part and re-keys them together. Closing it wants item-identity adoption:
before inserting, look the identifiers up and adopt the item already holding them. That is
a different transaction from the entity one (`resolveAndLinkEntities` runs after the item
row exists), so it does not fall out of the work already done.

## No artist analogue of the aux-art backfill

Artist-rung art is fetched inside the identity pass, so it is gated by that pass's
enrichment marker: an artist already marked enriched is out of the queue and gets no
imagery until a `--force` run. The release-group rung does not have this problem, because
a separate backfill phase (`auxArtNeededPredicate`) re-asks about a group whose front is
settled but whose auxiliary slots are empty.

Someone injecting an art provider into an already-enriched library therefore has to run
`waxbin enrich --force` once, which re-asks MusicBrainz about every artist to get at the
art. The fix is an artist backfill phase shaped like the release-group one: queue artists
with an mbid, no whole-entity art lock, and an empty slot, and ask only the art providers.
It was not built with the rung itself because the release-group phase's marker vocabulary
(`aux_art`) and its ghost heuristics would need an artist-scoped twin, which is more new
machinery than the rung it would serve.
