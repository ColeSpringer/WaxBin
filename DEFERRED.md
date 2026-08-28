# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## `resolveReleaseGroup`

Enrichment fills a group's or an album's mbid column without re-keying the row, and
the resolvers look rows up by match_key alone, so a file that later arrives carrying
that same MusicBrainz id computes an mbid key, misses, and inserts a second row for
the identity the first one already holds. The pair splits silently: stars, art, and
curation stay on the old heuristic row while new members accumulate on the fresh
mbid-keyed one, and nothing finds the split (audit has no duplicate-mbid scan, db
verify compares counts, and release_group.mbid carries no unique index; adding one
would only turn the duplicate insert into a scan failure). The album rung documents
its half of this as merge's job, but no one is told a merge is owed. Closing it wants
resolve-time adoption: before inserting an mbid-keyed row, look the id up in the mbid
column and re-key the existing row onto the id, keeping its pid and attachments. A
duplicate-mbid finder in audit would be the detection half if adoption proves too hot
for the resolve path.
