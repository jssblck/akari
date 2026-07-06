-- Partial feed and sort indexes for the default browse feed, which hides subagents.
--
-- The global Sessions view defaults to top-level work: ListAllSessions adds
-- "s.relationship_type <> 'subagent'" to every default-feed query (see read_sessions.go).
-- The existing feed and sort indexes (0033 last_active_at, 0014 message_count/total_tokens,
-- 0028 total_cost_usd) cover every session, subagents included. On a fleet where spawned
-- reviewers and fan-out workers vastly outnumber the top-level runs, walking one of those
-- indexes to fill a 100-row page skips over long runs of hidden subagent rows, and a tail
-- page with few remaining work items scans the rest of the history to confirm there are no
-- more. The cost of a page then grows with the corpus, not with the bounded page.
--
-- These partial twins carry only the top-level rows, in the same (col, id) shape the full
-- indexes use, so the planner can satisfy the default feed's ordered page as an index walk
-- that stops at the LIMIT and never visits a subagent row. The predicate matches the query's
-- own "relationship_type <> 'subagent'" exactly, which is what lets the planner use them;
-- relationship_type is NOT NULL (default ''), so the predicate is null-safe and every
-- top-level row is present. Each (col, id) btree serves both sort directions the UI's
-- click-to-sort reaches, the same way the full indexes do (see SessionFilter.orderClause).
--
-- The IncludeSubagents feed (subagents=1) and the count/facet probes still read the full
-- indexes, so those keep their coverage; this only adds the top-level-only path the default
-- feed takes. Indexes change no derived output, so this needs no parse.Epoch bump.
--
-- IF NOT EXISTS keeps this replayable on a database whose schema already carries these
-- objects but whose schema_migrations does not record the version (a schema-only dump
-- restore), matching the posture of migrations 0014 and 0028.
CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_feed_active
  ON sessions(last_active_at DESC, id DESC)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_messages_sort
  ON sessions(message_count, id)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_tokens_sort
  ON sessions(total_tokens, id)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_cost_sort
  ON sessions(total_cost_usd, id)
  WHERE relationship_type <> 'subagent';
