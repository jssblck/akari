-- Per-project public overview: let any signed-in user expose a project's usage
-- overview (totals, the activity grid, the by-model/by-agent breakdowns, and the
-- quality/tools/churn band) to logged-out viewers at a stable URL keyed on the
-- project id (/p/<id>).
--
-- This mirrors the per-user overview publicity (migration 0015): the project is
-- addressed by its own id, so no separate capability id is needed. A single boolean
-- gates whether the public page resolves. Disabling flips the gate off (the link
-- 404s) without changing the address, so re-enabling brings the same /p/<id> back.
--
-- The page is aggregate only and scoped to the named project. Unlike the signed-in
-- project page it lists no sessions (those stay private unless published one at a
-- time) and omits the by-user breakdown, so it exposes neither a session nor which
-- accounts ran in the repo.
--
-- The IF NOT EXISTS guard keeps this migration replayable on a database whose schema
-- already carries the column but whose schema_migrations does not record the version
-- (a schema-only dev-seed restore), matching 0015's posture.
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS overview_public BOOLEAN NOT NULL DEFAULT FALSE;
