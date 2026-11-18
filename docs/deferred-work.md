# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered.

## A cover no file states can outlast its file on an item with several files

A scan that finds a file without a cover takes back the item's tag or sidecar front only
once no other file of the item states that picture (`dropUnstatedFrontTx`), and it decides
that at a file's read. Two states keep a front nothing on disk states. The file that stated
it leaves the item (deleted, trashed, retagged to another recording) while the item's other
files are unchanged, so they take the fast path and nothing is read. Or what a file of the
item states is unknown (`file.front_hash` is NULL): a write-back stripped its picture, or
every read of it since said nothing, as an import's does and as a scan does that could not
read a cover file. Neither reaches an item backed by one file, which follows the disk at
once, and a forced scan reads every file and settles both. Dropping the front when the
stating file leaves would close the first, but a file that is not the primary never gives
the item its cover, so a trashed file restored as an alternate would come back to an item
that had lost its cover for good. Closing it means deciding when another file's cover
becomes the item's, which a copy's read does not do today.
