-- 0044: index epoch staleness on the attempted epoch, not raw parser_epoch.
--
-- A deterministic parse failure keeps parser_epoch at the last SUCCESSFUL
-- rebuild (0043), so failed sessions sit inside the "parser_epoch behind the
-- running epoch" btree range forever, even while their recorded failure pins
-- them out of the due set. Every hot staleness probe (the due scan's epoch
-- branch, the drain's opening count, the FleetStatus EXISTS, the OG snapshot
-- gate) then filters those rows one by one, and the cost of "is a fleet
-- rebuild draining?" grows with the corpus's accumulated failure history
-- rather than with the actual backlog.
--
-- The attempted epoch folds the pin into the indexed value: it is the epoch
-- of the last successful rebuild, raised to the failure epoch while a
-- recorded failure still covers the session's current bytes. A session whose
-- parse failed at the running epoch indexes AT that epoch and falls outside
-- the behind-range entirely, so the steady-state probes scan an empty range
-- no matter how many failures the corpus carries. New bytes break the byte
-- match, the expression falls back to parser_epoch, and the session re-enters
-- the range (and the due set) at once. The expression must stay semantically
-- identical to store's attemptedEpoch (internal/server/store/due.go) for the
-- planner to match it.
CREATE INDEX idx_session_raw_attempted_epoch ON session_raw (
  (CASE WHEN parse_error <> '' AND parse_error_byte_len = byte_len
        THEN GREATEST(parser_epoch, parse_error_epoch)
        ELSE parser_epoch END)
);

-- The plain parser_epoch btree (0042) served the old two-range predicates;
-- nothing queries bare parser_epoch ranges anymore (the due scan's remaining
-- parser_epoch <= $1 is a filter over candidates the other indexes produce).
DROP INDEX idx_session_raw_parser_epoch;
