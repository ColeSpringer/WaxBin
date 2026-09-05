# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## The listening log takes no recorded time

`StartSession` and `EndSession` stamp `play_session` with the wall clock, so imported
history reaches `play_state` (a play carries its own time through `MarkPlayed` now) but
never the log that `stats --year` reads: a household moving in from Last.fm gets its
plays counted and its recency right, and an empty year in review. Nothing asks for it
yet, since sessions are not proxied and WaxDeck keeps its own listen log, so the shape
waits for a consumer: a `RecordSession` write taking `started_at`, `ended_at`, and
`ms_played` as recorded values, beside the open/close pair.
