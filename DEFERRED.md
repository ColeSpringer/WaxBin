# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## An album with no barcode gets no release id

A release group is the album as a concept; a release is one edition of it, and a
popular record has dozens. Enrichment resolves the group from title and artist, but
the editions under it share the title, the artist, the track list, and usually the
track count, so almost nothing WaxBin knows about a local rip tells them apart.

Barcode and label catalog number do, because they are printed per edition, and those
are what `album.mbid` is matched on. A library tagged without them gets a release
group and no release. Why counts and year are not used as a fallback is settled and
lives in `enrich/release.go`.

Closing it needs media format and country, which genuinely separate a group's
editions and which WaxBin does not store at all (`library.media` is unrelated: it is
what content a root holds). That is a column, a scan-side source to fill it, and a
read surface, so it is a feature rather than a matcher tweak.

The practical cost is small: a consumer wanting cover art can use
`/release-group/<mbid>/front`, which `enrich/coverart.go` already calls. What is lost
is per-edition detail, that pressing's label, country, date, and its own art.
