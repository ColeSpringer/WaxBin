# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands. A gap that waits on WaxLabel or WaxFlow is an ask rather
than a gap, and it goes in [upstream-requests.md](upstream-requests.md) instead;
an entry here that depends on one names it.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## Group fronts have no backfill

A release group's front is asked for only while its identity is walked, so once the
group's marker settles nothing asks again short of `enrich --force-phase release-group`,
which re-reads MusicBrainz for every group at one request a second. Three things leave a
settled group without a front that a later ask could fill: a cover provider registered
after the group settled, one an `EnrichmentProviderList` hook left out of the pass that
resolved it, and a front that failed again on the pass that asked it a second time. The
album rung has `album-art` for exactly this and the auxiliary roles have `aux-art`; a
front walk over settled groups with no front and no art lock, asking the cover providers
at the release-group rung, would close it without any MusicBrainz traffic and would give
those fronts the retry window misses get. It is a new phase with its own marker type,
queue, count, CLI label and docs, so it is its own change.

## Two write-back tests assume back-to-back fills get different stamps

`TestEnrichmentWritebackOwedUntilSettled` and `TestEnrichedAlbumLabelFiles` (store/sqlite)
fill a field, settle the file at that fill's stamp, and fill again moments later, then
expect the second fill to read as newer. Field provenance is stamped with `nowNS`, so on a
clock that ticks every 15.6ms, as Windows' does, both fills can share a stamp and the
second never reads as owed. Coarsening `nowNS` in store/sqlite/store.go (the recipe in
the Windows notes) fails both every time, on the committed code as much as on later
changes; Windows CI has not failed on them so far. A real pass never fills, settles and
fills again inside one tick, so it is the tests' premise, but either stamping provenance
through `Store.stampNS` or giving the tests distinct stamps would close it.
