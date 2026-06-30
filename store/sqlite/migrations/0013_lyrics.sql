-- WaxBin schema v13: structured lyrics (synced + unsynced).
-- Lyrics are read-side data keyed by item. WaxBin parses a sibling .lrc sidecar
-- directly (the authoritative source when present) and reads embedded USLT
-- (unsynced) and SYLT (synced) tags through WaxLabel at scan time. The DB row is
-- authoritative for reads; synced lines are stored as JSON [{ms,text}] in time
-- order. A row exists only when the file carried some lyric content, so the table
-- stays sparse.

CREATE TABLE lyrics (
  item_id    INTEGER PRIMARY KEY REFERENCES playable_item(id) ON DELETE CASCADE,
  source     TEXT    NOT NULL,           -- 'lrc' (sidecar) | 'embedded'
  synced     INTEGER NOT NULL DEFAULT 0, -- 1 when timed lines are present
  unsynced   TEXT,                       -- plain unsynchronized text (USLT)
  lines      TEXT,                       -- JSON [{"ms":N,"text":"..."}], time-ordered
  updated_at INTEGER NOT NULL
);
