# Upstream requests

The standing list of things WaxBin wants from the sibling Wax repos it depends
on. Only wax-series dependencies belong here.

## WaxLabel

No open requests.

## WaxFlow

- **An unquoted operand keeps only its first token.** `cue.fields` splits a line
  on whitespace, treating a double-quoted run as one token, and the handler for
  `TITLE`, `PERFORMER`, and `ISRC` stores `args[0]`. So an unquoted `TITLE Jazz
  Album` reads as `Jazz`: the rest of the line is parsed as tokens and then
  dropped. Quotes are optional in the format and plenty of hand-written sheets
  go without them, and WaxBin's retired parser took the remainder of the line.
  The value lands in a virtual track's title, a book chapter's label, and the
  album title a cue rip catalogs under, so the loss is user-visible. Wanted: the
  remainder of the line for the single-operand commands, as the format means it.
  Shipped workaround: none; the truncated title is what the catalog stores.
  `meta.TestParseCueSheetUnquotedTitleTakesOneToken` pins the present behaviour
  and fails the day it changes.

- **One malformed timestamp refuses the whole sheet.** `Parse` is syntactic by
  design, so a single unreadable `INDEX` line returns an error and no sheet at
  all, where WaxBin's retired parser dropped that one track and kept the rest.
  A new rip with one typo in one INDEX therefore yields no virtual tracks and a
  book sheet yields no chapters, rather than losing the one track that was
  mistyped. Wanted: a tolerant mode, or a sheet returned alongside the per-line
  errors, so a reader that is not splitting can use what parsed. Shipped
  workaround: the refusal is recorded as a `cue_track_dropped` diagnostic naming
  the line, and a rip already carved keeps its tracks until the sheet reads again;
  `scan.TestScanCueRefusedSheetKeepsTheFile` and
  `scan.TestScanRefusedSheetKeepsAnExistingRip`.

- **A sheet with no FILE line is refused.** `Parse` rejects a `TRACK` that comes
  before any `FILE` ("TRACK before any FILE"), which is right for a sheet that
  indexes a rip and wrong for the two sheets WaxBin reads most: a `.cue` sidecar
  named after the one audio file beside it, and a hand-written chapter sheet fed
  to `chapters set --file`. Both imply their file. Wanted: an implied single FILE
  when a sheet names none, or an option for it. Shipped workaround: WaxBin
  prepends `FILE "" WAVE` when the sheet declares no FILE, after stripping any
  byte-order mark (upstream strips one only at the very start), and takes the
  extra line back out of a refusal's line number;
  `meta.TestParseCueSheetToleratesAMissingFILE` pins both halves.
