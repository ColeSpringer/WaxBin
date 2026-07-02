-- WaxBin schema v24: correct chapter.source values mislabeled by v22.
-- v22 added chapter.source with DEFAULT 'embedded', which retroactively stamped
-- every pre-existing chapter as 'embedded', including rows that were never embedded.
-- Because precedence ranks embedded above cue and podcast_url, those mislabels let a
-- poorer or placeholder source shadow a richer real one on read. This migration
-- restores the correct source for the two identifiable cases and leaves genuine
-- embedded book chapters untouched.

-- 1) Synthetic single whole-file placeholders. For a book part with no embedded
-- chapters the scanner emits ONE open-ended chapter spanning 0..0 (scan.bookInput),
-- marked 'synthetic' so a sibling .cue outranks it. v22 relabeled these as 'embedded',
-- which then wrongly outranks the .cue. Restore them by matching the synthetic shape: a
-- book part's SOLE chapter with start_ms = 0 AND end_ms = 0, meaning it has no real
-- bounds.
UPDATE chapter
SET source = 'synthetic'
WHERE source = 'embedded'
  AND start_ms = 0
  AND end_ms = 0
  AND book_item_id IN (SELECT id FROM playable_item WHERE kind = 'book')
  AND (book_item_id, file_id) IN (
        SELECT book_item_id, file_id
        FROM chapter
        GROUP BY book_item_id, file_id
        HAVING COUNT(*) = 1
      );

-- 2) Podcast episode chapters. An episode only ever receives chapters from its
-- Podcasting-2.0 chapters JSON (source 'podcast_url'); there is no embedded-chapter
-- path for episodes. So any episode chapter still labeled 'embedded' is a pre-v22 row
-- the default mislabeled. Restore 'podcast_url' so a re-sync replaces it in place
-- instead of leaving a stale duplicate beside the fresh rows.
UPDATE chapter
SET source = 'podcast_url'
WHERE source = 'embedded'
  AND book_item_id IN (SELECT id FROM playable_item WHERE kind = 'episode');
