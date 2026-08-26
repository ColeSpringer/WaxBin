# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

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
