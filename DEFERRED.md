# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## A failed enrichment tag write is not retried

`writeEnrichmentTags` reopens only the items whose newest enrichment provenance is at or
after the pass start (`enrichedTagSelect`), and an album label folded into a member's
write is struck from the label fan-out before that write is attempted. A file whose write
fails (a read-only mount, a transient error) therefore keeps its values catalog-only until
some later pass happens to write that item again, and `--force` does not reach it, since
the fills are fill-when-empty and a filled field's `updated_at` never moves. Fixing it
means recording the pending write per file, a diagnostic the next pass reads or a marker
the successful write clears, rather than inferring it from provenance timestamps.

## The album label fan-out plans every enriched album on every write-tags pass

`enrichmentAlbumLabelEdits` fetches every enrichment label ever written and runs one
`EntityMemberFiles` query per album to build the fold-in plan, then fans out only the
recent ones on their own. The plan is deliberate (a file being rewritten anyway takes its
album label for free), but it costs a query per enriched album per pass. One query joining
the label curation rows to their member files would replace the loop.
