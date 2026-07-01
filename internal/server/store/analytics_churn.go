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
// apply.
type ChurnFile struct {
	Path     string
	Edits    int
	Sessions int
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

// FileChurn computes which files a scope edited repeatedly. It dedupes replayed edit calls
// with the shared cohort partition (see dedupToolCallsPartition), keeps only edits that
// carry a parsed file path (the projection stores an edit whose path did not parse as NULL
// via nullString, so file_path IS NOT NULL drops them, the same guard the per-session
// edit_churn signal uses so an unattributable edit invents no thrash), groups by path, and
// keeps the paths edited more than once. The list is capped at the busiest files while the
// churned-file count comes from the whole set, so the panel can report the dropped tail.
func (s *Store) FileChurn(ctx context.Context, f AnalyticsFilter) (FileChurn, error) {
	var fc FileChurn

	filter, args := f.clauseFor("s.started_at")
	rows, err := s.Pool.Query(ctx,
		`WITH scoped AS (
		   SELECT tc.session_id, tc.message_ordinal, tc.call_index, tc.tool_name,
		          tc.file_path, tc.input_sha256, tc.result_status, tc.call_uid
		     FROM tool_calls tc
		     JOIN sessions s ON s.id = tc.session_id
		    WHERE tc.category = 'edit' AND tc.file_path IS NOT NULL`+filter+`
		 ),
		 ranked AS (
		   SELECT file_path, session_id,
		          row_number() OVER (
		            PARTITION BY `+dedupToolCallsPartition+`
		            ORDER BY message_ordinal, call_index
		          ) AS rn
		     FROM scoped
		 ),
		 agg AS (
		   SELECT file_path,
		          count(*) AS edits,
		          count(DISTINCT session_id) AS sessions
		     FROM ranked WHERE rn = 1
		    GROUP BY file_path
		   HAVING count(*) > 1
		 )
		 SELECT file_path, edits, sessions, count(*) OVER () AS churned
		   FROM agg
		  ORDER BY edits DESC, file_path`, args...)
	if err != nil {
		return FileChurn{}, fmt.Errorf("query file churn: %w", err)
	}
	defer rows.Close()

	var churned int
	for rows.Next() {
		var cf ChurnFile
		if err := rows.Scan(&cf.Path, &cf.Edits, &cf.Sessions, &churned); err != nil {
			return FileChurn{}, fmt.Errorf("scan file churn: %w", err)
		}
		if len(fc.Files) < maxChurnFiles {
			fc.Files = append(fc.Files, cf)
		}
	}
	if err := rows.Err(); err != nil {
		return FileChurn{}, fmt.Errorf("iterate file churn: %w", err)
	}
	if churned > maxChurnFiles {
		fc.Clipped = churned - maxChurnFiles
	}
	return fc, nil
}
