-- Partial facet indexes for the default browse feed under a project, user, agent, or machine.
--
-- Migration 0046 added top-level-only twins of the GLOBAL sort indexes, so the unfaceted feed
-- walks only top-level rows. It left a gap for the faceted feed. The feed's facet chips narrow
-- ListAllSessions by one of s.project_id, s.user_id, s.agent, or s.machine, and 0033 already
-- indexes each of those with (facet, last_active_at DESC, id DESC). But those 0033 indexes
-- carry every session, subagents included, so the default recency order under a selective facet
-- has no top-level-only index to walk: the planner either picks a 0033 facet index and scans the
-- facet's accumulated subagent history to fill a page (the facet is a tight index cond, so it
-- looks cheap, but on a fan-out-heavy project most of what it walks is hidden subagents), or
-- picks 0046's global top-level index and treats the facet as a filter, scanning other facets'
-- rows to find this one's. Either way a 100-row page can read far more than 100 rows.
--
-- These partial twins carry only the top-level rows in the same (facet, last_active_at DESC,
-- id DESC) shape, so a faceted default-order page is a single index walk that is both selective
-- on the facet and free of subagent rows, and it stops at the LIMIT. The predicate matches the
-- query's own "relationship_type <> 'subagent'" exactly, which is what lets the planner use it;
-- relationship_type is NOT NULL (default ''), so it is null-safe and every top-level row is
-- present. Each (facet, col, id) btree serves both sort directions the UI's click-to-sort
-- reaches, the same way the 0033 twins do (see SessionFilter.orderClause).
--
-- Only the default order (last_active_at) gets these per-facet twins, because it is the feed's
-- always-on path: every feed load, project page, and drill lands on it. The explicit sort
-- orders (message_count, total_tokens, total_cost_usd) are user-initiated and comparatively
-- rare, and they already avoid the subagent scan through 0046's global top-level sort indexes
-- with the facet as a bounded post-filter. Giving each of them a per-facet twin would add three
-- more churning compound indexes per facet whose keys (cost, tokens, message count) move on
-- every projection rebuild, taxing the ingest hot path to serve an occasional click. So the
-- faceted explicit sorts stay on the global top-level twins by design; only the default order,
-- where the read cost is paid on every page, earns the dedicated per-facet index.
--
-- The IncludeSubagents feed (subagents=1) and the count/facet probes still read the full 0033
-- indexes, so those keep their coverage; this only adds the top-level-only path the default
-- faceted feed takes. Indexes change no derived output, so this needs no parse.Epoch bump.
--
-- IF NOT EXISTS keeps this replayable on a database whose schema already carries these objects
-- but whose schema_migrations does not record the version (a schema-only dump restore),
-- matching the posture of migrations 0033 and 0046.
CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_project_feed_active
  ON sessions(project_id, last_active_at DESC, id DESC)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_user_feed_active
  ON sessions(user_id, last_active_at DESC, id DESC)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_agent_feed_active
  ON sessions(agent, last_active_at DESC, id DESC)
  WHERE relationship_type <> 'subagent';

CREATE INDEX IF NOT EXISTS idx_sessions_toplevel_machine_feed_active
  ON sessions(machine, last_active_at DESC, id DESC)
  WHERE relationship_type <> 'subagent';
