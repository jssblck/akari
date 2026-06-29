-- Incrementally maintained facet counts for the global Sessions filter rail.
--
-- The rail shows the busiest agents, machines, users, and projects with a
-- session count each. Computing those with GROUP BY over the whole sessions
-- table on every page load is O(total sessions) per request. Instead we keep a
-- small rollup, updated by a row trigger as sessions are inserted, deleted, or
-- re-attributed, and read a bounded top-N from it at request time.
--
-- key holds the facet value: the agent/machine string directly, or the
-- user_id / project_id as text (resolved to a username / project at read time).

CREATE TABLE session_facets (
  kind  TEXT   NOT NULL CHECK (kind IN ('agent', 'machine', 'user', 'project')),
  key   TEXT   NOT NULL,
  n     BIGINT NOT NULL,
  PRIMARY KEY (kind, key)
);

-- Busiest-first reads per kind ride this index; the rollup is small (one row per
-- distinct value), so the top-N read never touches the sessions table.
CREATE INDEX idx_session_facets_rank ON session_facets(kind, n DESC, key);

-- The trigger increments the new row's facets and decrements the old row's, so an
-- INSERT counts up, a DELETE counts down, and an UPDATE that moves a session
-- (e.g. project re-attribution) shifts the count; unchanged fields net to zero.
CREATE OR REPLACE FUNCTION akari_bump_session_facets() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'INSERT' OR TG_OP = 'UPDATE' THEN
    IF NEW.agent <> '' THEN
      INSERT INTO session_facets(kind, key, n) VALUES ('agent', NEW.agent, 1)
        ON CONFLICT (kind, key) DO UPDATE SET n = session_facets.n + 1;
    END IF;
    IF NEW.machine <> '' THEN
      INSERT INTO session_facets(kind, key, n) VALUES ('machine', NEW.machine, 1)
        ON CONFLICT (kind, key) DO UPDATE SET n = session_facets.n + 1;
    END IF;
    INSERT INTO session_facets(kind, key, n) VALUES ('user', NEW.user_id::text, 1)
      ON CONFLICT (kind, key) DO UPDATE SET n = session_facets.n + 1;
    INSERT INTO session_facets(kind, key, n) VALUES ('project', NEW.project_id::text, 1)
      ON CONFLICT (kind, key) DO UPDATE SET n = session_facets.n + 1;
  END IF;
  IF TG_OP = 'UPDATE' OR TG_OP = 'DELETE' THEN
    IF OLD.agent <> '' THEN
      UPDATE session_facets SET n = n - 1 WHERE kind = 'agent' AND key = OLD.agent;
    END IF;
    IF OLD.machine <> '' THEN
      UPDATE session_facets SET n = n - 1 WHERE kind = 'machine' AND key = OLD.machine;
    END IF;
    UPDATE session_facets SET n = n - 1 WHERE kind = 'user' AND key = OLD.user_id::text;
    UPDATE session_facets SET n = n - 1 WHERE kind = 'project' AND key = OLD.project_id::text;
    -- Drop values that no longer back any session so the rollup stays bounded by
    -- the live distinct values, not the historical ones.
    DELETE FROM session_facets WHERE n <= 0;
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- UPDATE OF restricts the trigger to facet-relevant columns, so the frequent
-- projection updates during live ingest (message_count, token totals, ...) do
-- not churn the rollup.
CREATE TRIGGER trg_session_facets
  AFTER INSERT OR DELETE OR UPDATE OF agent, machine, user_id, project_id ON sessions
  FOR EACH ROW EXECUTE FUNCTION akari_bump_session_facets();

-- Backfill from any sessions that already exist.
INSERT INTO session_facets(kind, key, n)
  SELECT 'agent', agent, count(*) FROM sessions WHERE agent <> '' GROUP BY agent
  UNION ALL
  SELECT 'machine', machine, count(*) FROM sessions WHERE machine <> '' GROUP BY machine
  UNION ALL
  SELECT 'user', user_id::text, count(*) FROM sessions GROUP BY user_id
  UNION ALL
  SELECT 'project', project_id::text, count(*) FROM sessions GROUP BY project_id;
