-- WaxBin schema v17: denormalized book total duration.
-- A book's running time is the sum of its parts' durations. Storing it on the
-- book row lets the shared item view and the queryable duration_ms field report
-- the whole book without a per-row correlated subquery, and keeps display, filter,
-- and sort consistent for a multi-file book. It is write-maintained (recomputed
-- whenever the book's part set changes: scan and trash) and checked by db verify.
ALTER TABLE book ADD COLUMN total_duration_ms INTEGER NOT NULL DEFAULT 0;

-- Backfill existing books from their current parts, using each part's effective
-- duration (the larger of its file duration and its furthest chapter offset) so the
-- stored total matches the chapter timeline and never drifts on first verify.
UPDATE book SET total_duration_ms = (
  SELECT COALESCE(SUM(MAX(
    COALESCE(f.duration_ms, 0),
    COALESCE((SELECT MAX(MAX(c.start_ms, c.end_ms)) FROM chapter c
              WHERE c.book_item_id = itf.item_id AND c.file_id = itf.file_id), 0)
  )), 0)
  FROM item_file itf JOIN file f ON f.id = itf.file_id
  WHERE itf.item_id = book.item_id
);
