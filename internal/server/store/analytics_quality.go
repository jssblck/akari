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
	return QualityDistribution{
		Grades:   orderedCounts(gradeOrder, grades),
		Outcomes: orderedCounts(outcomeOrder, outcomes),
		Sessions: gTotal,
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
