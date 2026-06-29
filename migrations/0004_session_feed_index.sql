-- Feed-order index for the session lists. The project view, the global Sessions
-- view, and the Overview's recent-activity feed all order by updated_at then id
-- and take a bounded page (LIMIT). This composite index lets Postgres satisfy
-- that order with an index scan and stop at the limit, instead of sorting the
-- whole sessions table on every request as the corpus grows.
CREATE INDEX idx_sessions_feed ON sessions(updated_at DESC, id DESC);
