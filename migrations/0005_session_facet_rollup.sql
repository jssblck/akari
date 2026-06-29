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
  -- Aggregate the row change into signed per-(kind,key) deltas: +1 for the new
  -- row's facet values, -1 for the old row's. An unchanged field nets to zero
  -- and is skipped (HAVING). Applying the upsert in (kind,key) order makes every
  -- concurrent writer take row locks in the same sequence, so two opposite
  -- re-attributions (A->B and B->A) can never deadlock.
  WITH delta(kind, key, d) AS (
    SELECT kind, key, sum(d) AS d
      FROM (
                  SELECT 'agent'   AS kind, NEW.agent           AS key,  1 AS d WHERE TG_OP <> 'DELETE' AND NEW.agent <> ''
        UNION ALL SELECT 'machine',         NEW.machine,                 1      WHERE TG_OP <> 'DELETE' AND NEW.machine <> ''
        UNION ALL SELECT 'user',            NEW.user_id::text,           1      WHERE TG_OP <> 'DELETE'
        UNION ALL SELECT 'project',         NEW.project_id::text,        1      WHERE TG_OP <> 'DELETE'
        UNION ALL SELECT 'agent',           OLD.agent,                  -1      WHERE TG_OP <> 'INSERT' AND OLD.agent <> ''
        UNION ALL SELECT 'machine',         OLD.machine,                -1      WHERE TG_OP <> 'INSERT' AND OLD.machine <> ''
        UNION ALL SELECT 'user',            OLD.user_id::text,          -1      WHERE TG_OP <> 'INSERT'
        UNION ALL SELECT 'project',         OLD.project_id::text,       -1      WHERE TG_OP <> 'INSERT'
      ) s
     GROUP BY kind, key
    HAVING sum(d) <> 0
  )
  INSERT INTO session_facets(kind, key, n)
    SELECT kind, key, d FROM delta ORDER BY kind, key
  ON CONFLICT (kind, key) DO UPDATE SET n = session_facets.n + EXCLUDED.n;

  -- Only an old value can be driven to zero. Delete just those (already locked)
  -- rows by primary key, so the cleanup neither scans the whole rollup nor takes
  -- a lock out of order.
  IF TG_OP <> 'INSERT' THEN
    DELETE FROM session_facets sf
     WHERE sf.n <= 0
       AND ( (sf.kind = 'agent'   AND sf.key = OLD.agent)
          OR (sf.kind = 'machine' AND sf.key = OLD.machine)
          OR (sf.kind = 'user'    AND sf.key = OLD.user_id::text)
          OR (sf.kind = 'project' AND sf.key = OLD.project_id::text) );
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
