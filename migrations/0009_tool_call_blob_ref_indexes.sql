-- Hash-leading indexes for the blob references a tool call holds. A tool call points at
-- the CAS through input_sha256 and result_sha256, both foreign keys to blobs(sha256), but
-- Postgres does not index a referencing column on its own, and the only tool_calls index
-- so far leads with (session_id, call_uid). Two hot paths probe these columns by hash and
-- so scan every tool call in a session without these indexes:
--   - serving a blob authorizes it with SessionReferencesBlob, whose tool_calls arm is
--     session_id = $1 AND (input_sha256 = $2 OR result_sha256 = $2). Lifting Codex images
--     to the CAS means a session view now issues a blob request per rendered image as well
--     as per tool body, so an image-heavy session runs many such probes against an
--     accumulating tool_calls set: O(images * tool calls) on every open or live refresh.
--   - the orphan sweep tests each blob with input_sha256 = b.sha256 OR result_sha256 =
--     b.sha256, a probe on the hash alone.
-- Leading each index with the hash serves the sweep's hash-only probe, and session_id
-- second turns the authorization arm into a two-column equality lookup. The OR over the
-- two columns becomes two index probes the planner combines, so each authorization is
-- logarithmic in the session's blob references rather than a full scan.
CREATE INDEX idx_tool_calls_input_sha_session
  ON tool_calls (input_sha256, session_id);
CREATE INDEX idx_tool_calls_result_sha_session
  ON tool_calls (result_sha256, session_id);
