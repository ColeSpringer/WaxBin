# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands. A gap that waits on WaxLabel or WaxFlow is an ask rather
than a gap, and it goes in [upstream-requests.md](upstream-requests.md) instead;
an entry here that depends on one names it.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## The album and artist art backfills record the front with the auxiliary roles

The `album-art` and `artist-art` backfills keep one marker per target, matched when any
image landed. A target whose front no provider had, but whose auxiliary role another
provider filled, is marked matched, and a matched marker is durable: the retry window that
re-asks a miss never re-asks it, so a cover a provider gains later is reached only through
new evidence (a landed mbid, a curation clear) or `enrich --force-phase`. The same marker
keeps a provider serving the auxiliary roles that joins after a front was filled from
reaching the target. Either takes an injected provider serving the auxiliary roles. The
`group-art` backfill already keeps its two halves apart (the `group_front` and `group_art`
markers, `model.ArtHalf`); closing this means doing the same at the other two rungs.

## The artist rung asks a picture-less artist twice in one pass

The artist identity walk asks the cover providers at the artist rung for an artist's
front on the way past, and the `artist-art` backfill later in the same pass asks the
`CapArtistArt` providers about any slot still empty. A provider advertising both
`CapCover` and `CapArtistArt` there is asked twice for an artist it has no picture for,
once per walk. The group rung avoids this by leaving a vacant front to its backfill alone.
The artist rung cannot yet, since a provider written before `CapArtistArt` advertises
`CapCover` alone and is reached only through the identity walk. Closing it means deciding
which walk owns a vacant artist front and which capability reaches it there, which
changes what an injected provider has to advertise.
