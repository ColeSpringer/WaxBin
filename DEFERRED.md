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
