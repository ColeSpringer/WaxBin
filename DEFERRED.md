# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## `thumb_cache` has no size surface and no prune

A generated derivative is reclaimed only when its source is, through the cascade on
`art_source`. There is nothing that reports how much space the table holds and nothing
that prunes by age or size, so a library browsed at a large rung grows the catalog by a
derivative per cover per rung and `db verify` reports it as healthy, which it is.

This became worth naming when a sized resolve started re-encoding covers held in a
format most clients cannot display: those derivatives are full-size rather than
thumbnail-size, and the box is passed to the generator unclamped, so rungs above a
source's own size no longer share one entry. See the comment above the `s.thumbnail`
call in `store/sqlite/art.go` for why the stored dimensions are not trusted to do that
collapsing.

Left for its own change because a report needs a `db` subcommand surface and a prune
needs a retention policy, and neither belongs in the art resolver.
