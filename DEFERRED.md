# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## `ResolveArt` caches one entry per requested size, with no ladder

The box a caller asks for is the cache key, and nothing rounds it. A client asking at
a fixed set of rungs holds a handful of derivatives per cover. One that sizes to a
layout box instead holds a derivative per distinct pixel width that box has ever had,
so a resized window or a responsive grid mints rows nothing will ask for again. The
in-process LRU keys the same way and holds 256 entries, so the same traffic evicts
thumbnails that are still in use.

`db thumbs` bounds the table after the fact and is the only thing that does. It
reclaims the waste rather than preventing it, which leaves the growth bounded by how
often someone remembers to prune.

The fix is to round a requested box up to a fixed ladder and serve that rung, which
collapses the key space to a few entries per cover. It is left for its own change
because it changes what `ResolveArt` hands back: a caller asking for 187 would get a
200-wide picture, so the ladder has to be chosen against how clients scale the result,
and the blob has to carry the size actually served rather than the size requested. See
the comment above the `s.thumbnail` call in `store/sqlite/art.go` for why the box
cannot instead be clamped to the source's own stored dimensions.
