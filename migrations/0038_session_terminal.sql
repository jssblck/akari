-- terminal marks a session the client declared finished at upload time, the
-- server-side half of `akari sync --finalize`. A Codex session's final turn has no
-- closing user line, so the client withholds it until the file has been idle for the
-- settle window; on an ephemeral host (CI, a cloud sandbox) that window never elapses
-- before teardown, so --finalize forces the trailing turn out. That fixes data
-- completeness. This column fixes timeliness: a session is normally gradeable only
-- once it has been idle past the abandoned threshold (30 minutes), so even a complete
-- transcript would not carry a grade until long after the host is gone. terminal is
-- OR'd into the two server-side idle checks (the idleLongEnough fact gatherSignalFacts
-- feeds quality.Classify, and the dueSettledBatch settle predicate) so a session the
-- host called terminal is gradeable immediately, regardless of the idle window.
--
-- It is honest metadata beyond the grading shortcut: it records that a session ran on
-- an ephemeral host and was closed out deliberately rather than left idle, which
-- analytics can later use to tell CI and sandbox runs apart from interactive ones.
--
-- It defaults false, so every existing session and every ordinary (watch-loop) sync
-- keeps the idle-window behavior untouched; only a --finalize announce sets it. The
-- announce upsert OR's the incoming value onto the stored one, so the flag is sticky:
-- a later non-finalize re-announce of the same session never clears a terminal marker.
--
-- The IF NOT EXISTS guard keeps this migration replayable on a schema-only dev dump
-- that already carries the column but does not record the migration version.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS terminal BOOLEAN NOT NULL DEFAULT false;

-- The settle pass grades terminal sessions in a dedicated drain (dueTerminalBatch),
-- separate from the settled-by-idle drain, because a terminal session is gradeable
-- regardless of its ended_at: the client asserted it is finished, and its transcript may
-- carry no parseable timestamp (a NULL ended_at) yet still have gradeable messages. The
-- settled drain keyset-pages by (ended_at, id), which cannot order a NULL ended_at, so the
-- terminal drain keyset-pages by id alone:
--   WHERE signals_stale AND terminal AND id > cursor
--   ORDER BY id
-- This partial index carries exactly the terminal stale rows in id order, so that scan is
-- an index range read over only the due rows, never a walk of the whole stale tail. The set
-- it indexes is tiny and short-lived: the finalize refresh grades a terminal session
-- seconds after upload and clears signals_stale, so a row drops out of this index almost
-- immediately; it keeps the settle-pass backstop cheap for the window before that refresh
-- lands (or if it never does, when the settle loop is the only path). IF NOT EXISTS keeps it
-- replayable on a schema-only dev dump that already carries the index.
CREATE INDEX IF NOT EXISTS idx_sessions_terminal_stale
  ON sessions (id)
  WHERE signals_stale AND terminal;
