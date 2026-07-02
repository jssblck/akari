-- signals_stale marks a session whose stored signals (session_signals) no longer match its
-- current projection, so the settle pass finds the sessions that actually need grading by an
-- index seek rather than re-examining the whole settled history on every wake.
--
-- Before this, RefreshSettledSignals scanned the settled tail in ended_at order and evaluated
-- a four-part "due" predicate (missing row, stale version, graded before the settle point,
-- graded before the last projection change) as a post-filter joined to session_signals. That
-- filter cannot be indexed across the two tables, so each wake walked every already-graded
-- settled session to reach the few newly due ones: O(settled history) per tick, quadratic as
-- the corpus grows. A single-table boolean the projection maintains collapses that to an index
-- seek over only the due rows.
--
-- The flag is maintained where the projection changes: applyAggregates and the reparse reset
-- set it true (the projection moved, so any stored grade is now behind), and refreshSignalsTx
-- clears it only when the session is settled (a reparse that grades a still-live session leaves
-- it true, so the settle pass re-grades it once the outcome stabilizes). A quality-version bump
-- is reconciled by RefreshSettledSignals marking stale-version rows before it drains, seeking
-- them through the version-leading session_signals indexes.
--
-- It defaults true so every pre-existing session is treated as needing a grade until the first
-- settle pass or the epoch reparse (which clears it per session as it refreshes) reaches it.
-- That is the safe direction: a spurious true costs one recompute, a spurious false would
-- strand a session ungraded.
ALTER TABLE sessions ADD COLUMN signals_stale BOOLEAN NOT NULL DEFAULT true;

-- The settle pass selects settled, stale sessions oldest-first and keyset-pages by (ended_at,
-- id):
--   WHERE signals_stale AND ended_at IS NOT NULL AND ended_at < cutoff AND (ended_at, id) > ...
--   ORDER BY ended_at, id
-- This partial index carries only the stale rows in that order, so the pass seeks to the cutoff
-- and scans forward over the due rows alone, never the graded remainder. A session drops out of
-- the index the moment it is graded (signals_stale cleared) and re-enters on the next
-- projection change. IF NOT EXISTS keeps it replayable on a schema-only dev dump that already
-- carries the index.
CREATE INDEX IF NOT EXISTS idx_sessions_signals_stale
  ON sessions (ended_at, id)
  WHERE signals_stale;

-- The settle pass reconciles a quality.Version bump by marking every stale-version signals row
-- due (a version bump changes no projection, so the signals_stale flag alone cannot catch it).
-- That reconcile is an inequality scan (signals_version <> current) no index can seek, so it must
-- not run on every settle wake. This singleton marker on parse_meta records the version the corpus
-- was last reconciled at: the settle pass reads it (one row) and runs the scan only when it is
-- behind quality.Version, which is once per deploy that bumps the version. It defaults to 0, which
-- differs from any real quality.Version, so the first settle pass after this migration reconciles
-- once and advances it.
ALTER TABLE parse_meta ADD COLUMN signals_reconciled_version INT NOT NULL DEFAULT 0;
