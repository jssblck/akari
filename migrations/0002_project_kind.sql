-- Project kind: distinguish git-remote-backed projects from sessions whose
-- working directory has no usable git remote. A standalone project's folder
-- exists on disk but resolves to no origin; an orphaned project's folder is
-- unknown or gone. Both are keyed by machine + path rather than a remote, and
-- both are backed up rather than skipped. Existing rows are git-remote projects.

ALTER TABLE projects
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'remote'
    CHECK (kind IN ('remote', 'standalone', 'orphaned'));

CREATE INDEX idx_projects_kind ON projects(kind);
