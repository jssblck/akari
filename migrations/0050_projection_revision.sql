-- Public transcript pagination must detect any projection replacement between
-- requests. The raw byte cursor and parser epoch describe what a rebuild covered,
-- but neither changes when an operator repeats a rebuild over the same input.
-- Incrementing this revision in the rebuild transaction gives every committed
-- projection a stable identity without coupling cursors to message contents.
ALTER TABLE session_raw
  ADD COLUMN projection_revision BIGINT NOT NULL DEFAULT 0;
