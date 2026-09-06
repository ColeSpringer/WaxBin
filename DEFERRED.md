# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## A phase- or provider-scoped force

`--force` is the only way to ask a newly registered provider about targets whose
marker records a match. Register a background provider after Deezer has settled every
artist front and those markers stand, matched and durable, so nothing short of a
forced run re-asks; and a forced run re-asks every phase, not just the one the new
provider serves. Scoping it would need a `RunOptions` field, a CLI flag, and an
`EnrichParams` field with a protocol bump, so it is its own change rather than a rider
on the retry window.

## The album-art front half re-downloads the group cover, and has no toggle

The backfill asks the Cover Art Archive for a release's front for most identified
albums, and for the majority of them the answer is the same bytes as the group cover
already fetched. The content-addressed store dedups the bytes, not the download. The
archive's release-group JSON names the release its cover came from, so the phase could
skip the release fetch when that release is this album. Cheap, later.

The two halves also share one switch: `enrichment.cover_art` gates CapCover, which is
what registers the archive at all, so declining the per-release download means giving up
release-group covers too. A separate key is deliberately not added yet, because the
download above is the actual cost and skipping it would leave the knob with nothing to
buy. If the dedup lands and someone still wants the halves apart, add the key then.
