-- 0045: back off operational rebuild failures.
--
-- An operational rebuild failure (a store or CAS error: the transaction rolled
-- back, nothing was recorded) leaves the session due, which is the retry path.
-- But every chunk wake drains the WHOLE due set, so a backlog of sessions
-- whose operational failure does not clear on its own (the classic case is a
-- CAS blob the client lifted but never uploaded) was re-attempted on every
-- unrelated append, and that backlog can grow with the corpus. The retry
-- marker makes the due scan skip a failed session until its backoff elapses,
-- doubling from 30s to a 1h ceiling on consecutive failures.
--
-- Anything that changes the situation clears the marker for an immediate
-- retry: a successful rebuild, a recorded deterministic failure (the pin
-- supersedes the backoff), new bytes (the re-sync that finally uploads a
-- missing blob also appends), a raw reset, and an operator reparse.
--
-- The epoch-staleness gates deliberately ignore the marker: a backing-off
-- rebuild is deferred, not cancelled, so the corpus is still mixed and the
-- gate staying up is the honest answer.
ALTER TABLE session_raw ADD COLUMN parse_retry_at TIMESTAMPTZ;
ALTER TABLE session_raw ADD COLUMN parse_retry_backoff_secs INT NOT NULL DEFAULT 0;

-- Ready-work indexes. Deferring a retry only helps if the hot paths also stop
-- SEEING the deferred rows: a backing-off session is still byte-dirty and
-- still epoch-behind, so left in the general indexes it would be fetched and
-- filtered on every wake, and the per-append cost would again grow with the
-- corpus's failure backlog. The due scan therefore reads three ready-work
-- indexes, each excluding rows parked on a future retry:
--
--   - dirty_ready: byte-dirty sessions with no deferral (the every-chunk wake;
--     replaces 0043's idx_session_raw_dirty_bytes, which had no retry filter),
--   - epoch_ready: the attempted-epoch expression over undeferred rows (the
--     fleet-rebuild branch; same expression as 0044's full index),
--   - retry_elapsed: deferred rows keyed by when they become ready, so a drain
--     picks up exactly the retries whose backoff has elapsed via one range
--     scan and never visits the still-parked remainder.
--
-- 0044's full attempted-epoch index stays: the fleet gates (EpochStaleCount,
-- EpochStaleExists, the OG snapshot check) intentionally include deferred rows
-- and keep using it.
DROP INDEX idx_session_raw_dirty_bytes;
CREATE INDEX idx_session_raw_dirty_ready ON session_raw (session_id)
  WHERE parsed_byte_len <> byte_len AND parse_retry_at IS NULL;
CREATE INDEX idx_session_raw_epoch_ready ON session_raw (
  (CASE WHEN parse_error <> '' AND parse_error_byte_len = byte_len
        THEN GREATEST(parser_epoch, parse_error_epoch)
        ELSE parser_epoch END)
) WHERE parse_retry_at IS NULL;
CREATE INDEX idx_session_raw_retry_elapsed ON session_raw (parse_retry_at)
  WHERE parse_retry_at IS NOT NULL;
