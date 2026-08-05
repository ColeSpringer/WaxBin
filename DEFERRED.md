# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## Music credits are one artist per track

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

## `album.mbid` is never enriched

`lookupReleaseGroup` requests `inc=artist-credits+genres`, so the music path
structurally cannot see a release id. Deciding which release of a group a local rip is
(track count, barcode, media) is a matching problem, not a projection. Consequence: an
enriched-rather-than-tagged library carries `ReleaseGroupMBID` and no `AlbumMBID`.

## Dead columns

`series.mbid` and `album.edition` have no writer. Cheaper to remove than it looks
under the pre-1.0 single-migration policy, but `series.mbid` is read (it feeds
`read.EntityInfo.MBID` for a series), so dropping it is an API change rather than a
tidy-up.
