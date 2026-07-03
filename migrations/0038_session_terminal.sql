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

-- The settle pass's due predicate (dueSettledBatch) now reads
--   WHERE signals_stale AND ended_at IS NOT NULL
--     AND (terminal OR ended_at < now() - interval '30 minutes')
--     AND (ended_at, id) > cursor
--   ORDER BY ended_at, id
-- The existing idx_sessions_signals_stale serves the settled disjunct (an ended_at
-- range seek), but a terminal session that ended moments ago sits at the recent end of
-- that index, past the cutoff, so without help the OR would force a scan of the whole
-- stale tail every wake to find it. This partial index carries exactly the terminal
-- stale rows in (ended_at, id) order, so Postgres can bitmap-OR it with the settled
-- range and read only the due rows. The set it indexes is tiny and short-lived: the
-- finalize refresh grades a terminal session seconds after upload and clears
-- signals_stale, so a row drops out of this index almost immediately; it exists to keep
-- the settle-pass backstop cheap for the window before that refresh lands (or if it
-- never does, when the settle loop is the only path). IF NOT EXISTS keeps it replayable.
CREATE INDEX IF NOT EXISTS idx_sessions_terminal_stale
  ON sessions (ended_at, id)
  WHERE signals_stale AND terminal;
