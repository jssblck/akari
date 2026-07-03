package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/quality"
)

// LabeledCount is one bar in a distribution: a canonical key (the stored value, or ""
// for the unscored grade bucket) and how many sessions fell in it. The view maps the
// key to a display label and a colour; the store keeps the raw key so the two layers
// stay decoupled.
type LabeledCount struct {
	Key   string
	Count int
}

// QualityDistribution is the Insights page's quality summary over a scope: how the
// scoped sessions split across letter grades and across outcomes, plus the total. Every
// scoped session contributes to exactly one bucket of each split: a session whose
// signals row is missing or was written by an older scoring version (before its backfill
// reparse) reads as unscored and unknown rather than vanishing, so the splits cover the
// same session set the archetype distribution and the session count do, and the three
// reconcile. The parsed views are gated during a reparse, so a reader never sees a
// half-rebuilt distribution.
type QualityDistribution struct {
	Grades   []LabeledCount // canonical order: A, B, C, D, F, then "" (unscored)
	Outcomes []LabeledCount // canonical order: completed, errored, abandoned, unknown
	Sessions int            // total scoped sessions (every session falls in one bucket)
	// Graded is how many scoped sessions carry a gated (current-version, non-stale) grade,
	// the complement of the unscored bucket. The Insights Grades panel reads it as a
	// coverage figure ("N% graded"): the share of the cohort a letter grade actually
	// speaks for, so a distribution dominated by the unscored bar reads as thin coverage
	// rather than a real spread. It is Sessions minus the unscored count, but computed in
	// the same grade scan with a FILTER so it needs no second pass over the cohort.
	Graded int
}

// gradeOrder and outcomeOrder fix the bar order so a distribution reads the same every
// render (best to worst, common to rare) rather than in whatever order the GROUP BY
// happened to return. The empty grade is the unscored bucket, shown last.
var gradeOrder = []string{"A", "B", "C", "D", "F", ""}
var outcomeOrder = []string{
	string(quality.OutcomeCompleted),
	string(quality.OutcomeErrored),
	string(quality.OutcomeAbandoned),
	string(quality.OutcomeUnknown),
}

// QualityDistribution aggregates the scoped sessions' grades and outcomes. The grade split and the
// outcome split are separate scans, so it wraps them in one repeatable-read, read-only snapshot:
// the session total, the grade buckets, and the outcome buckets then all describe the same scoped
// cohort, where a concurrent session insert or signals_stale change between the two scans could
// otherwise pair a grade split from one cohort with an outcome split from another. Insights threads
// its own snapshot through qualityDistributionFrom so its panels reconcile against each other; this
// is the standalone equivalent. The snapshot takes no row locks, so it never blocks ingest.
func (s *Store) QualityDistribution(ctx context.Context, f AnalyticsFilter) (QualityDistribution, error) {
	var out QualityDistribution
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = s.qualityDistributionFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return QualityDistribution{}, fmt.Errorf("quality distribution snapshot: %w", err)
	}
	return out, nil
}

// qualityDistributionFrom aggregates the scoped sessions' grades and outcomes for the Insights
// page from one querier. It shares the analytics filter (clauseFor on s.started_at, so a
// windowed view counts sessions that started in the window). The two splits come from one scan
// each over the scoped sessions left-joined to their current-version signals, folded into the
// fixed canonical order with zero-filled buckets so every grade and outcome draws a comparable
// bar even at zero.
func (s *Store) qualityDistributionFrom(ctx context.Context, q querier, f AnalyticsFilter) (QualityDistribution, error) {
	grades, gTotal, err := s.scopedSignalCounts(ctx, q, f, "grade", "")
	if err != nil {
		return QualityDistribution{}, fmt.Errorf("grade distribution: %w", err)
	}
	outcomes, _, err := s.scopedSignalCounts(ctx, q, f, "outcome", string(quality.OutcomeUnknown))
	if err != nil {
		return QualityDistribution{}, fmt.Errorf("outcome distribution: %w", err)
	}
	// Graded is the cohort minus its unscored bucket. It falls straight out of the grade
	// scan already run (the "" key holds the sessions with no gated grade), so it needs no
	// FILTER clause or second pass: subtracting the one bucket the same scan produced is
	// exact and cheaper than re-counting.
	return QualityDistribution{
		Grades:   orderedCounts(gradeOrder, grades),
		Outcomes: orderedCounts(outcomeOrder, outcomes),
		Sessions: gTotal,
		Graded:   gTotal - grades[""],
	}, nil
}

// scopedSignalCounts groups the scoped sessions by one signals column (grade or outcome)
// and returns the per-key counts plus the total. It scopes over sessions and LEFT JOINs
// the current-version signals row, so a session whose row is missing or stale folds into
// the missing bucket rather than dropping out: that keeps the count equal to the scoped
// session total and reconciles the grade and outcome splits with the archetype split. The
// join also requires the row to be usable (NOT s.signals_stale), so a session that gained an
// appended region after its last grade, or was graded while still live, reads as unscored/unknown
// until the settle pass re-grades it, rather than counting a grade for an earlier or not-yet-
// settled state; that is the read-side mirror of the signals_stale flag the settle pass drains
// on. The flag rather than a refreshed_at >= updated_at comparison is deliberate: updated_at also
// moves on metadata-only writes that leave the grade valid, and the flag is set at exactly the
// projection-change sites, so it is the precise staleness signal. col and missing are internal
// constants (the column name and the bucket a session with no usable row reads as, "" for grade
// or "unknown" for outcome), never caller input, so interpolating them is safe.
func (s *Store) scopedSignalCounts(ctx context.Context, q querier, f AnalyticsFilter, col, missing string) (map[string]int, int, error) {
	filter, args := f.clauseFor("s.started_at")
	args = append(args, quality.Version)
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT coalesce(sig.%s, '%s'), count(*)
		   FROM sessions s
		   LEFT JOIN session_signals sig
		     ON sig.session_id = s.id AND sig.signals_version = $%d AND NOT s.signals_stale
		  WHERE TRUE`+filter+`
		  GROUP BY 1`, col, missing, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int{}
	total := 0
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, 0, fmt.Errorf("scan signal count row: %w", err)
		}
		out[key] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate signal counts: %w", err)
	}
	return out, total, nil
}

// orderedCounts projects a key->count map onto a fixed key order, zero-filling missing
// keys so every canonical bucket draws a bar. Keys outside the canonical order (none
// expected, since the columns are CHECK-constrained) are dropped rather than appended,
// keeping the bar set stable.
func orderedCounts(order []string, counts map[string]int) []LabeledCount {
	out := make([]LabeledCount, 0, len(order))
	for _, k := range order {
		out = append(out, LabeledCount{Key: k, Count: counts[k]})
	}
	return out
}

// maxUserQualityRows caps the per-user quality table at its busiest authors, so an
// instance with many accounts still reads as a short leaderboard of who ran the most
// sessions in the window. The count of authors beyond the cap rides UserQualityStats.Clipped
// so the panel can note the tail it dropped, the same top-N shape fileChurnFrom uses.
const maxUserQualityRows = 12

// UserQuality is one author's quality picture over a scope: how many sessions they ran, how
// many carry a gated grade, how those sessions split across outcomes, and their average
// quality score. The outcome counts partition Sessions (Unknown is the residue, so the four
// always sum to Sessions), which lets the panel draw one stacked magnitude bar per row. AvgScore
// is nil when no session in scope is scored, so the panel dashes it rather than printing a zero
// that would read as a real (bad) average.
type UserQuality struct {
	Username  string
	Sessions  int
	Graded    int
	Completed int
	Abandoned int
	Errored   int
	Unknown   int
	AvgScore  *float64 // nil when no scored session in scope
}

// UserQualityStats is the per-user quality leaderboard: the busiest authors' rows plus a count
// of the authors clipped past the cap, so the panel can note the tail. Users is ordered by
// session count descending then username, so the reader scans from the heaviest author down and
// ties read alphabetically rather than in arbitrary GROUP BY order.
type UserQualityStats struct {
	Users   []UserQuality
	Clipped int
}

// userQualityFrom aggregates each author's session count, outcome mix, graded coverage, and
// average score for the Insights People panel. It scopes over sessions with the same analytics
// filter the distributions use (clauseFor on s.started_at, so a windowed view counts sessions
// that started in the window and a per-project or per-user scope narrows identically), joins the
// author, and LEFT JOINs the gated signals row so an ungraded session still counts under its
// author with an 'unknown' outcome and no score rather than dropping out. Every figure derives
// from that one gated join with FILTER aggregates, so the row is one scan per author.
//
// The gate is the shared idiom (sig.signals_version = quality.Version AND NOT s.signals_stale):
// a missing, superseded, or stale row reads as ungraded, so Graded and AvgScore speak only for
// the sessions a current grade actually covers, matching the distribution buckets and the
// session-list drill-down. Unknown is Sessions minus the three known outcomes, computed in Go so
// the four counts partition Sessions exactly (a coalesced 'unknown' from the join and the residue
// are the same set). AvgScore rounds to one decimal and is left nil when the author has no scored
// session in scope, the "unmeasured" default a zero would misread as a real average.
//
// The busiest maxUserQualityRows authors are kept, so the cap lives in SQL: the query LIMITs to
// that many rows and Postgres returns the top slice by a bounded top-N sort rather than streaming
// every author for the loop to discard. The clipped-tail count needs the whole-set total, which a
// windowed count(*) OVER () could not give under a LIMIT, so it comes from a scalar count over the
// same grouped set (agg is referenced twice, so Postgres materializes it once and the grouping runs
// a single time).
func (s *Store) userQualityFrom(ctx context.Context, q querier, f AnalyticsFilter) (UserQualityStats, error) {
	var out UserQualityStats

	filter, args := f.clauseFor("s.started_at")
	versionArg := len(args) + 1
	args = append(args, quality.Version)
	limitArg := len(args) + 1
	args = append(args, maxUserQualityRows)
	rows, err := q.Query(ctx, fmt.Sprintf(
		`WITH agg AS (
		   SELECT u.username AS username,
		          count(*) AS sessions,
		          count(*) FILTER (WHERE sig.grade IS NOT NULL) AS graded,
		          count(*) FILTER (WHERE sig.outcome = 'completed') AS completed,
		          count(*) FILTER (WHERE sig.outcome = 'abandoned') AS abandoned,
		          count(*) FILTER (WHERE sig.outcome = 'errored') AS errored,
		          round(avg(sig.score) FILTER (WHERE sig.score IS NOT NULL), 1) AS avg_score
		     FROM sessions s
		     JOIN users u ON u.id = s.user_id
		     LEFT JOIN session_signals sig
		       ON sig.session_id = s.id AND sig.signals_version = $%d AND NOT s.signals_stale
		    WHERE TRUE`+filter+`
		    GROUP BY u.username
		 )
		 SELECT username, sessions, graded, completed, abandoned, errored, avg_score,
		        (SELECT count(*) FROM agg) AS authors
		   FROM agg
		  ORDER BY sessions DESC, username
		  LIMIT $%d`, versionArg, limitArg), args...)
	if err != nil {
		return UserQualityStats{}, fmt.Errorf("query user quality: %w", err)
	}
	defer rows.Close()

	var authors int
	for rows.Next() {
		var uq UserQuality
		if err := rows.Scan(&uq.Username, &uq.Sessions, &uq.Graded,
			&uq.Completed, &uq.Abandoned, &uq.Errored, &uq.AvgScore, &authors); err != nil {
			return UserQualityStats{}, fmt.Errorf("scan user quality: %w", err)
		}
		// Unknown is the residue: sessions whose gated outcome is 'unknown' (or absent), so the
		// four outcome counts partition Sessions and the stacked bar spans the full width.
		uq.Unknown = uq.Sessions - uq.Completed - uq.Abandoned - uq.Errored
		out.Users = append(out.Users, uq)
	}
	if err := rows.Err(); err != nil {
		return UserQualityStats{}, fmt.Errorf("iterate user quality: %w", err)
	}
	if authors > maxUserQualityRows {
		out.Clipped = authors - maxUserQualityRows
	}
	return out, nil
}

// AvgQualityScore is the mean quality score across the scoped sessions that carry a
// gated (current-version, non-stale) grade, or nil when none is scored. It shares the
// analytics filter (clauseFor on s.started_at, so a windowed scope counts sessions that
// started in the window) and the same signals gate the quality distribution uses, so it
// speaks for exactly the graded cohort the Insights Grades panel counts rather than a
// different set. The public project OG card reads it and rounds it to a representative
// letter grade (via quality.GradeFor), so the card's single QUALITY figure summarizes the
// same graded sessions the page's grade distribution draws. It is nil, not zero, when no
// scored session is in scope, so the card can dash an unmeasured figure rather than print
// a zero that would read as a real (failing) average.
func (s *Store) AvgQualityScore(ctx context.Context, f AnalyticsFilter) (*float64, error) {
	return s.avgQualityScoreFrom(ctx, s.Pool, f)
}

// avgQualityScoreFrom is AvgQualityScore over one querier, so the standalone pooled read and
// the project card's reparse-gated snapshot (ProjectCardSnapshot) run the identical query on
// the same MVCC snapshot as the card's token totals rather than a second pooled connection
// that could straddle a reparse. It returns nil, not zero, when no scored session is in scope.
func (s *Store) avgQualityScoreFrom(ctx context.Context, q querier, f AnalyticsFilter) (*float64, error) {
	filter, args := f.clauseFor("s.started_at")
	args = append(args, quality.Version)
	var avg *float64
	// Scope the average to the exact graded cohort the Insights Grades panel counts: the panel
	// (scopedSignalCounts on grade) defines graded as grade IS NOT NULL, so this matches it with
	// the same predicate. Migration 0040 makes score and grade a consistent pair: both set or both
	// NULL, and a set grade equals GradeFor(score). So every row in this grade-IS-NOT-NULL cohort
	// carries a score (no silently-skipped NULL score), and each row's stored grade agrees with the
	// letter its score bands to: the card's representative grade (GradeFor of the mean score) and
	// the panel's stored-grade distribution are drawn from the same graded sessions under one
	// score->grade mapping, so they reconcile rather than describing subtly different cohorts.
	err := q.QueryRow(ctx, fmt.Sprintf(
		`SELECT avg(sig.score)::float8
		   FROM sessions s
		   JOIN session_signals sig
		     ON sig.session_id = s.id AND sig.signals_version = $%d AND NOT s.signals_stale
		  WHERE sig.grade IS NOT NULL`+filter, len(args)), args...).Scan(&avg)
	if err != nil {
		return nil, fmt.Errorf("avg quality score: %w", err)
	}
	return avg, nil
}
