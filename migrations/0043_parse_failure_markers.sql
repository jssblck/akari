-- Split "this parse attempt failed" from "this rebuild succeeded". Epoch 0042's
-- scheme stamped a deterministic reducer failure onto parsed_byte_len and
-- parser_epoch, the same columns a successful rebuild stamps, so a malformed
-- session's PRIOR projection read as current after an epoch bump: it dropped out
-- of the due set, the epoch-staleness gates, and the fleet progress count while
-- serving rows derived by the old parser.
--
-- parser_epoch and parsed_byte_len now always describe the last successful
-- rebuild. A failed attempt records what it tried instead: the error text
-- (parse_error, already present) plus the epoch and raw length the attempt
-- covered. A session is skipped by the due scan only while the recorded failure
-- matches its current bytes and the running epoch exactly; new bytes or a new
-- epoch retry it, and a successful rebuild clears all three markers.
ALTER TABLE session_raw
    ADD COLUMN parse_error_epoch INT NOT NULL DEFAULT 0,
    ADD COLUMN parse_error_byte_len BIGINT NOT NULL DEFAULT 0;

-- The worker's due scan runs on every chunk wake, and its byte-dirty half
-- (parsed_byte_len <> byte_len) had no index: finding the one dirty live session
-- scanned the whole corpus per append. Rows enter this partial index exactly
-- while dirty and leave when the rebuild stamps them covered, so in steady state
-- it is near-empty and the scan touches only the sessions that actually need
-- work; the parser_epoch btree (0042) carries the epoch half.
CREATE INDEX idx_session_raw_dirty_bytes ON session_raw (session_id)
    WHERE parsed_byte_len <> byte_len;
