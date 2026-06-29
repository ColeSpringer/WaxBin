-- WaxBin schema v4: an index for the analyze pass's file selection.
-- FilesNeedingAnalysis filters to audio files and pages by (rel_path, id); a
-- partial index on audio rows in rel_path order lets the planner seek and scan
-- in order instead of full-scanning + filesorting the whole file table on every
-- analyze run. The index implicitly carries the rowid (id), supporting the
-- (rel_path, id) keyset tiebreak.
CREATE INDEX file_audio_relpath ON file(rel_path) WHERE kind = 'audio';
