-- The parse pipeline becomes rebuild-on-dirty: a background worker rebuilds a
-- session's whole projection from its raw bytes whenever the bytes or the parser
-- epoch have moved, and the incremental machinery this schema supported goes away
-- (see docs/DESIGN.md "Server-side parsing pipeline"). This migration drops the
-- incremental bookkeeping and adds the one column the new pipeline needs.
--
-- parser_epoch is the parse.Epoch the last successful rebuild ran at. A session is
-- due for a rebuild when parsed_byte_len <> byte_len (new bytes) or parser_epoch
-- differs from the binary's constant (the parser changed). The DEFAULT 0 is below
-- any real epoch, so every existing session reads as due on the first boot after
-- this migration and the corpus rebuilds through the ordinary worker: that rebuild
-- is what re-derives every projection row, re-grades signals, and re-prices usage,
-- so nothing dropped below needs a data backfill.
ALTER TABLE session_raw ADD COLUMN parser_epoch INT NOT NULL DEFAULT 0;

-- Serves the two epoch-keyed scans: the worker's due scan and the OG-card
-- epoch-staleness gate. Both are written as range predicates (parser_epoch < $1
-- OR parser_epoch > $1) so this btree answers them; in steady state every row
-- sits at the running epoch, both ranges are empty, and the gate's EXISTS is an
-- index-only touch.
CREATE INDEX idx_session_raw_parser_epoch ON session_raw (parser_epoch);

-- The serialized reducer carry-state and its version. A rebuild always parses from
-- byte zero in one call, so reducer state lives in memory for the duration of the
-- parse and version blending (the reason parse_state_version existed) is impossible
-- by construction.
ALTER TABLE session_raw
    DROP COLUMN parse_state,
    DROP COLUMN parse_state_version;

-- parsed_byte_len keeps its name but changes meaning: it was the incremental parse
-- cursor, it is now the raw length the last successful rebuild covered. Reset it so
-- no session can read as current at the old cursor before its first rebuild; with
-- parser_epoch = 0 everywhere this is belt-and-suspenders, but it also restores the
-- parsed_byte_len <= byte_len invariant for any row a failed parse left mid-stream.
UPDATE session_raw SET parsed_byte_len = 0;

-- sessions.parser_version was the parse.Version stamp of the incremental path;
-- parser_epoch on session_raw replaces it.
ALTER TABLE sessions DROP COLUMN parser_version;

-- The cache-savings backfill machinery: the rollup is now re-folded by every
-- rebuild, and a reprice rides the epoch like any other derived-output change, so
-- there is no backfill candidate set to track.
DROP INDEX idx_sessions_cache_savings_candidate;
ALTER TABLE sessions DROP COLUMN cache_savings_backfilled;

-- The fleet-wide markers: reparsed_epoch is replaced by the per-session
-- parser_epoch, and the two reconcile markers guarded version machinery
-- (quality.Version, quality.PromptFactsVersion, pricing.Version) that no longer
-- exists; every derived representation now versions on parse.Epoch alone.
DROP TABLE parse_meta;

-- Per-row version stamps, same story: a rebuild recomputes prompt facts and
-- signals with the binary that runs it, and the epoch rebuild is what rolls a
-- classifier or scoring change across the corpus, so rows are never version-mixed
-- for longer than one rebuild and reads no longer gate on a stored version.
ALTER TABLE messages DROP COLUMN prompt_facts_version;
DROP INDEX idx_session_signals_grade;
DROP INDEX idx_session_signals_outcome;
ALTER TABLE session_signals
    DROP COLUMN signals_version,
    DROP COLUMN prompt_facts_version;
CREATE INDEX idx_session_signals_grade   ON session_signals (grade) WHERE grade IS NOT NULL;
CREATE INDEX idx_session_signals_outcome ON session_signals (outcome);
