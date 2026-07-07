package store

import (
	"context"
	"fmt"
)

// maxChurnFiles caps the churn list at the most-edited files, so a window that touched many
// files still reads as a short list of the real hotspots. The count of churned files is
// carried separately (see FileChurn.Clipped) so the panel can note the tail it dropped.
const maxChurnFiles = 10

// ChurnFile is one file's edit thrash over a scope: how many times it was edited and across
// how many sessions. The edits are deduped (a replayed transcript re-emits prior edits, so
// the raw rows over-count), the same dedup the tool analytics and the per-session signals
// apply. The file is keyed per project on its worktree-invariant relative path (the
// projection's file_rel_path, coalesced onto the absolute file_path when no relative form
// exists), so one repo file edited from several git worktrees reads as a single row rather
// than one per checkout. Project and ProjectID carry the owning project so the panel can
// label each bar and two projects that share a relative path stay distinct.
type ChurnFile struct {
	ProjectID int64
	Project   string // the project's display label (remote_key, or display_name for a standalone/orphaned project)
	Path      string
	Edits     int
	Sessions  int
}

// FileChurn is the cohort's edit-thrash picture: the files edited more than once in the
// window, the most-edited first. A file touched once is not churn and never appears. It is
// the fleet counterpart to the per-session edit_churn signal, pointing at the paths a fleet
// kept returning to (a sign of a hard spot, a flaky change, or a moving target).
type FileChurn struct {
	Files   []ChurnFile
	Clipped int // churned files beyond the shown list
}

// HasData reports whether any file was edited more than once, so the panel can show a note
// rather than an empty list for a window with no repeated edits.
func (c FileChurn) HasData() bool { return len(c.Files) > 0 }

// FileChurn computes which files a scope edited repeatedly on its own pooled connection for
// the Insights page. The snapshot path threads fileChurnFrom so every panel reads one MVCC
// snapshot.
func (s *Store) FileChurn(ctx context.Context, f AnalyticsFilter) (FileChurn, error) {
	return s.fileChurnFrom(ctx, s.Pool, f)
}

// fileChurnFrom computes which files a scope edited repeatedly, over the
// session_file_churn rollup of started_at-windowed sessions. The rollup already deduped
// replayed edit calls and dropped path-less edits at write time (deriveSessionRollupsTx,
// the same guards the per-session edit_churn signal uses), so the cohort read groups the
// per-session rows by (project, path) and keeps the pairs edited more than once.
//
// The grouping key is (s.project_id, fc.churn_path), where churn_path is the
// worktree-invariant file_rel_path coalesced onto the absolute file_path at derivation
// time: file_path alone is absolute, so the same repo file edited from several git
// worktrees of one repo would fragment into a row per checkout and the aggregate read as
// noise. Projects already collapse worktrees (they key on the canonical git remote), so
// pairing the two collapses those rows back into one. A path with no relative form (edited
// outside the workspace, or from a session with no announced cwd) rides its absolute
// file_path, so it still counts under its own name rather than vanishing; it just does not
// merge across worktrees. The project is joined for its display label (remote_key, or
// display_name for a standalone/orphaned project), the same CASE the session list sorts on.
//
// The panel shows only the busiest maxChurnFiles, so the cap belongs in SQL, not in Go: the
// query LIMITs to that many rows and Postgres returns just the top slice by a bounded top-N sort
// rather than sorting and streaming every churned path for the loop to discard all but the first
// few. The dropped-tail count still needs the whole-set total, which a count(*) OVER () window
// could not give under a LIMIT (the window would count only the returned rows), so it comes from
// a scalar (SELECT count(*) FROM agg) instead. agg is referenced twice (the top-N select and the
// count), which makes Postgres materialize it once, so the grouping runs a single time.
func (s *Store) fileChurnFrom(ctx context.Context, q querier, f AnalyticsFilter) (FileChurn, error) {
	var fc FileChurn

	filter, args := f.clauseFor("s.started_at")
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, maxChurnFiles)
	rows, err := q.Query(ctx,
		`WITH agg AS (
		   SELECT s.project_id, fc.churn_path,
		          sum(fc.edits)::bigint AS edits,
		          count(DISTINCT fc.session_id) AS sessions
		     FROM session_file_churn fc
		     JOIN sessions s ON s.id = fc.session_id
		    WHERE TRUE`+filter+`
		    GROUP BY s.project_id, fc.churn_path
		   HAVING sum(fc.edits) > 1
		 )
		 SELECT agg.project_id,
		        CASE WHEN p.kind IN ('standalone', 'orphaned') THEN p.display_name ELSE p.remote_key END AS project,
		        agg.churn_path, agg.edits, agg.sessions,
		        (SELECT count(*) FROM agg) AS churned
		   FROM agg
		   JOIN projects p ON p.id = agg.project_id
		  ORDER BY agg.edits DESC, project, agg.churn_path
		  LIMIT `+limitArg, args...)
	if err != nil {
		return FileChurn{}, fmt.Errorf("query file churn: %w", err)
	}
	defer rows.Close()

	var churned int
	for rows.Next() {
		var cf ChurnFile
		if err := rows.Scan(&cf.ProjectID, &cf.Project, &cf.Path, &cf.Edits, &cf.Sessions, &churned); err != nil {
			return FileChurn{}, fmt.Errorf("scan file churn: %w", err)
		}
		fc.Files = append(fc.Files, cf)
	}
	if err := rows.Err(); err != nil {
		return FileChurn{}, fmt.Errorf("iterate file churn: %w", err)
	}
	if churned > maxChurnFiles {
		fc.Clipped = churned - maxChurnFiles
	}
	return fc, nil
}
