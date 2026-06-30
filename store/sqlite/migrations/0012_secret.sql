-- WaxBin schema v12: provider credentials and other named secrets.
-- Values are stored plaintext; they are never logged or written to a logical
-- export. A full DB backup is a byte copy and therefore includes this table, so
-- backup exposes a redaction option for copies that leave the host.

CREATE TABLE secret (
  key        TEXT    PRIMARY KEY,
  value      TEXT    NOT NULL,
  updated_at INTEGER NOT NULL  -- unix nanoseconds
);
