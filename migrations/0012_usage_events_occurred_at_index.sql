-- The overview's date-range windows bound every usage rollup with
--   WHERE ue.occurred_at >= $since
-- across the daily series, the by-model split, and the windowed by-agent split.
-- Without an occurred_at index that lower bound has nothing to seek on, so each
-- bounded request seq-scans the whole accumulated usage_events table and the work
-- grows with total history rather than with the selected window.
--
-- This partial index seeks straight to the window's lower bound and skips the
-- undated events (occurred_at NULL), which never fall inside a window and so have
-- no place in the index. The bounded rollups become proportional to the events in
-- the window, not the events ever recorded.
CREATE INDEX idx_usage_events_occurred_at
  ON usage_events(occurred_at)
  WHERE occurred_at IS NOT NULL;
