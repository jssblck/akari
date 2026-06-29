-- A pending-only companion to idx_tool_calls_call_uid, for the tool-result
-- back-patch. That UPDATE is always
--   ... WHERE session_id = $1 AND call_uid = $2 AND result_status IS NULL
-- so it only ever resolves rows that have no result yet. Against the full
-- (session_id, call_uid) index, each back-patch probes every accumulated copy of a
-- replayed id and then discards the already-resolved ones, so a Claude turn replayed
-- K times (each replay re-delivering the same tool_use and tool_result) costs
-- O(K^2) index and heap visits even though every row is written only once.
--
-- This partial index holds only the unresolved rows. A row leaves it the moment its
-- result lands, so a later back-patch for the same id probes only the copies still
-- pending, which keeps the total back-patch work linear in the number of rows. The
-- full index stays: DuplicateCallUIDCount groups over every row per id (resolved or
-- not) to flag a session that repeated one, so it needs the unfiltered set.
CREATE INDEX idx_tool_calls_pending_result
  ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL AND result_status IS NULL;
