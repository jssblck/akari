-- Covering feed indexes for the filtered session lists. The global Sessions view
-- and the project view order by updated_at DESC, id DESC and take a bounded page,
-- optionally narrowed by one facet (project, agent, machine, or user). A plain
-- single-column index narrows but then forces a sort of the matching subset to
-- find the page; a composite (facet, updated_at DESC, id DESC) lets Postgres walk
-- the index already in feed order and stop at the limit, so a filtered request
-- reads a page, not the whole filtered set.
--
-- These supersede the plain project_id / user_id indexes (the composite's leading
-- column still serves equality lookups), so those are dropped to avoid carrying a
-- redundant index's write cost.

DROP INDEX idx_sessions_project;
DROP INDEX idx_sessions_user;

CREATE INDEX idx_sessions_project_feed ON sessions(project_id, updated_at DESC, id DESC);
CREATE INDEX idx_sessions_user_feed    ON sessions(user_id,    updated_at DESC, id DESC);
CREATE INDEX idx_sessions_agent_feed   ON sessions(agent,      updated_at DESC, id DESC);
CREATE INDEX idx_sessions_machine_feed ON sessions(machine,    updated_at DESC, id DESC);
