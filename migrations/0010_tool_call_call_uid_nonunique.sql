-- Drop the uniqueness on a session's tool-call ids. call_uid is the agent's own
-- tool_use id, and the back-patch that attaches a tool result keys on it. The
-- index was unique per session on the assumption that a call id is unique within a
-- session, but a resumed or compacted Claude transcript breaks that: it replays
-- prior assistant turns verbatim, so the same tool_use id legitimately rides more
-- than one row. Unique enforcement turned that replay into a parse abort (the
-- second insert tripped the index and rolled back the whole transaction), which
-- stalled reparse on four sessions and could never recover them.
--
-- With the index non-unique, both replayed copies keep their id and the back-patch
-- UPDATE ... WHERE call_uid = $1 stamps the same result onto each, which is what a
-- reader expects to see on a duplicated turn. The index still exists for that
-- back-patch lookup, just without the constraint. The accepted tradeoff: if a
-- session ever held two genuinely different calls that collided on one id (a
-- malformed reuse, not a replay), the single result would land on both. That case
-- is very rare in practice, and the session view now flags any session that carries
-- a duplicate id so it is visible if it turns out to be common.
--
-- No parser version bump: a session that contained a duplicate id could never have
-- parsed under the old unique index (it aborted), so no already-parsed projection
-- changes shape. The four stalled sessions sit at parse cursor 0 (their failed
-- reparse committed the reset but rolled back the advance), so a plain reparse now
-- carries them to completion.
DROP INDEX idx_tool_calls_call_uid;
CREATE INDEX idx_tool_calls_call_uid ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL;
