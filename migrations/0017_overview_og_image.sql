-- Open Graph preview cards for published usage overviews.
--
-- When an account publishes its overview (/u/<username>), a link unfurler wants a
-- preview image. Rendering that card on every crawl would recompute a year of
-- analytics per request; instead the server renders it on demand the first time the
-- card is fetched, caches the PNG bytes here, and serves the cache for a TTL. The
-- card is a snapshot (the activity heatmap plus the total-token and session
-- figures), so a cached copy that trails the live page by up to the TTL is exactly
-- right for a share preview.
--
-- Keyed one-to-one on the user: an account has at most one current card. The bytes
-- are stored inline as BYTEA rather than in the content-addressed blob store,
-- because the card is a small (~30 KB), mutable, per-user artifact with no sharing
-- or dedup to gain from CAS, and it is replaced wholesale on each re-render. The row
-- is cascaded away with the user, and generated_at drives the TTL (a fetch past the
-- window re-renders) and the cleanup sweep (an expired card is pruned).
--
-- The IF NOT EXISTS guard keeps this migration replayable on a database whose
-- schema already carries the table but whose schema_migrations does not record the
-- version (a schema-only dev-seed restore), matching 0014/0015's posture.
CREATE TABLE IF NOT EXISTS overview_og_images (
  user_id      BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  png          BYTEA NOT NULL,
  generated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
