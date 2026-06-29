-- parse_meta records, fleet-wide, the parser epoch the stored projection was last
-- rebuilt under. The server compares parse.Epoch (a binary constant) against
-- reparsed_epoch on startup: when they differ it reparses every session and writes
-- the new epoch back. The trigger lives in the binary rather than in a migration
-- because parser output often changes with no schema change at all (PR #18 added
-- Codex image payloads to the projection without a migration), so a
-- migration-versioned signal would miss exactly those changes.
--
-- The table is a singleton: the id column is a boolean fixed to TRUE by its CHECK,
-- so there can only ever be the one row, and reads and writes need no WHERE beyond
-- it. reparsed_epoch defaults to 0, which differs from parse.Epoch (1) on a fresh
-- database, so a brand-new server treats its (empty) corpus as needing a reparse
-- and converges to the current epoch on first start.
CREATE TABLE parse_meta (
  id             BOOLEAN PRIMARY KEY DEFAULT TRUE,
  reparsed_epoch INT NOT NULL DEFAULT 0,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT parse_meta_singleton CHECK (id)
);

INSERT INTO parse_meta (id) VALUES (TRUE) ON CONFLICT DO NOTHING;
