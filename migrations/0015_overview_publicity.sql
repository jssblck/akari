-- Per-user public overview: let an account expose its own usage overview (totals,
-- the activity grid, and the by-model/by-agent breakdowns) to logged-out viewers
-- at a stable, human-readable URL keyed on the username (/u/<username>).
--
-- Unlike the session publicity model (visibility + an unguessable public_id on
-- sessions, migration 0001), the overview is addressed by username, so no separate
-- capability id is needed: the URL is the username and a single boolean gates
-- whether it resolves. Disabling flips the gate off (the link 404s) without
-- changing the address, so re-enabling brings the same /u/<username> back. The
-- page is aggregate only and scoped to the named account, so it exposes neither
-- another user's usage nor any session.
--
-- The IF NOT EXISTS guard keeps this migration replayable on a database whose
-- schema already carries the column but whose schema_migrations does not record
-- the version (a schema-only dev-seed restore), matching 0014's posture.
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS overview_public BOOLEAN NOT NULL DEFAULT FALSE;
