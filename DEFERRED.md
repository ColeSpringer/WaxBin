# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## A book's description, subtitle, and edition have no on-disk tag key

The book fields walk fills all three, and `meta.BookFieldTagKeys` has no entry for any
of them, so `enrich --write-tags` leaves them in the catalog alone. A retag that forces
a full rescan therefore clears them, which is exactly the loss the write-back exists to
prevent for the fields beside them. Fixing it means picking keys the audiobook scanner
would read back (a TXXX/freeform pair per field) and wiring both halves, reader and
writer, since a key only the writer knows is worse than none.

## `album.year` is written at insert and never topped up

The scan sets `album.year` when it creates the row and no later member updates it, so an
mbid-keyed album (whose key ignores the year) can hold a NULL year over members that all
carry one. Found while designing the album fields walk's year veto, which works around
it: the walk refuses to fill a year when any member already has one, because the uniform
whole-album edit it would use would overwrite those tagged years. A scan-side fix would
top the column up from the members the way the other denormalized album columns are
maintained, and the veto could then read the column alone.
