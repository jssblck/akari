-- Open Graph preview cards for the two public pages that gained them beside the
-- per-user overview: a project's published overview (/p/<id>) and a published
-- session (/s/<public_id>).
--
-- These mirror overview_og_images (migration 0017) exactly, one table per entity so
-- each card is cascaded away with the row it previews rather than sharing a
-- polymorphic table that could not carry a foreign key. A link unfurler wants a
-- preview image; rendering it on every crawl would recompute a card per request, so
-- the server renders on demand the first time the card URL is fetched, caches the PNG
-- bytes here, and serves the cache for a TTL. generated_at drives both the TTL (a
-- fetch past the window re-renders) and the cleanup sweep (an expired card is pruned).
--
-- The project card is a snapshot of the same trailing-year usage the /p/<id> page
-- renders (the activity heatmap plus the total-token and session figures), keyed
-- one-to-one on the project. The session card is a snapshot of one session's own
-- rollups (its title, tokens, cost, message count, and grade), keyed one-to-one on
-- the session. The bytes are stored inline as BYTEA rather than in the
-- content-addressed blob store, matching 0017: each card is a small (~30 KB),
-- mutable, per-entity artifact with no sharing or dedup to gain from CAS, replaced
-- wholesale on each re-render.
--
-- The IF NOT EXISTS guard keeps this migration replayable on a database whose schema
-- already carries the tables but whose schema_migrations does not record the version
-- (a schema-only dev-seed restore), matching 0017's posture.
CREATE TABLE IF NOT EXISTS project_og_images (
  project_id   BIGINT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  png          BYTEA NOT NULL,
  generated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS session_og_images (
  session_id   BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  png          BYTEA NOT NULL,
  generated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
