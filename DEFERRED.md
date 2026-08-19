# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## Library root identity is byte-exact, so Windows case respellings can duplicate a library

EnsureLibrary matches a root by raw bytes (libraryByRootDB) and config.Validate
normalizes only through filepath.Abs, which preserves the case the user typed. On
Windows, where NTFS compares case-insensitively, registering C:\Music and later
re-running with c:\Music inserts a second library row over the same tree.
fileByPathDB has the same byte-exact shape. The CLI's --library resolver already
folds case (pathx.SamePath), so the two layers currently disagree.

Deferred because the fix is a design decision, not a patch: raw path bytes as
identity is a deliberate seam (non-UTF-8 roots on POSIX depend on it), and folding
at the store would need a canonical-case normalization step on Windows (there is
no cheap syscall that returns a path's on-disk casing without walking it) plus a
repair for catalogs that already hold both spellings.

## An entity art lock with no cover attached is invisible and hard to clear

`art set --type podcast <pid> --clear` clears the cover and records the lock (the
default), which is the state that stops a feed refilling it. Nothing then shows that
lock: ArtRoles reports Locked only while iterating rows that exist, so an entity with
no art rows reports nothing at all, and EntityCuration refuses podcast, playlist and
genre because entityTableFor covers only the three scalar-editable entities. Every
later `art set` on that entity is refused, and the way out is `--no-lock --force`.

The item side does not have this problem: FieldProvenance overlays a lock row that has
no artifact behind it, so `waxbin provenance` shows it.

Deferred because the fix is a read-surface decision rather than a patch. ArtRoles
returns one entry per stored role and has nowhere to put an entity-level flag, and
widening EntityCuration to the art entity types would also have to answer how `entity
show` renders an artifact lock among scalar fields (it currently prints `art user true`
with an empty value, and `entity edit --set art=` then rejects it).

## A locked show cover can leave the feed's image URL permanently ahead of the art

`podcast.image_url` serves two masters. UpsertFeed advances it from the feed on every
sync, while `podcast.upsert` reads it back as "the cover we already fetched" and skips
the download when it matches. With a locked cover the attach is skipped but the URL
still advances, so after unlocking, every later sync computes B == B, passes a nil
image, and the show keeps the old cover until the publisher rotates the URL again.

Deferred because the fix is a semantic choice about that column. Comparing against
`art_map.source_url` instead would re-fetch on every sync while the cover stays locked;
not advancing `image_url` when the lock skipped the attach needs the skip plumbed back
out of UpsertFeed. The related staleness where a rotated URL carries identical bytes is
already handled: attachEntityArtTxChanged re-attributes in place (refreshArtProvenanceTx).
