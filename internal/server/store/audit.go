package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditSummary is the Overview's audit read: the fleet verdict a team lead scans
// first (how much work the agents took on, how much of it shipped, how good it was,
// and what the failures cost), plus a short ranked list of the sessions most worth
// opening. It is scoped by the same AnalyticsFilter the usage panel uses, so the
// verdict and the panel below it describe the same window and the same users.
//
// Every count is over top-level work items only (relationship_type <> 'subagent'):
// a team lead audits the tasks their agents were handed, and a spawned reviewer or
// fan-out worker is an implementation detail that already rolls up under its parent.
// The spend figures the panel already carries (Analytics.TotalCost) stay fleet-wide
// so the money reconciles with the heatmap; only the audit's own WastedUSD is scoped
// to the failed top-level runs.
type AuditSummary struct {
	// WorkItems is the count of top-level sessions in scope: the tasks the fleet took on.
	WorkItems int
	// Settled is the work items that reached a terminal outcome (completed, errored, or
	// abandoned). A still-live or unscored session is not settled, so the completion rate
	// speaks only for work that has actually finished rather than counting in-flight runs
	// as failures.
	Settled int
	// Completed and Wasted partition the settled set: Completed finished cleanly, Wasted
	// errored or was abandoned. Settled minus these two is zero by construction.
	Completed int
	Wasted    int
	// Graded is the work items carrying a current (non-stale) letter grade, and GradePoints
	// is the sum of their grade points on the app's A=4..F=0 scale (the same scale the
	// Insights GPA line uses). GPA divides the two; both ride here so the view can show the
	// coverage the average speaks for rather than a bare number.
	Graded      int
	GradePoints float64
	// WastedUSD is the money the errored and abandoned top-level runs burned, the figure the
	// Spend tile pulls out of its total as "on failed runs". It reads the same base the Spend
	// total does (usage_events priced by cost_usd and dated by occurred_at, under the same
	// window and scope), narrowed to events whose session failed and is itself a top-level work
	// item. Because it only adds predicates to the sum behind Analytics.TotalCost, it is always
	// a subset of that total and cannot exceed the aggregate it annotates, and a run that started
	// before the window but spent tokens inside it counts here exactly as it counts in Spend.
	//
	// The relationship_type <> 'subagent' predicate keeps this the root run's own events, not the
	// whole work item's (root plus its subagent subtree, see TreeRollup): a subagent's events
	// carry the subagent's session id and drop out, so this is direct spend, a lower bound on the
	// true cost of failure. That also keeps it one indexed scan of the window's usage_events
	// rather than a recursive per-corpus subtree walk on the hot Overview path, and it matches the
	// fleet-wide Spend total, which counts every subagent event once under its own session.
	// TestOverviewAuditCostsAreDirect pins that a failed root's subagent spend is not folded in.
	// WastedIncomplete carries Analytics.CostIncomplete's marker: some failed run in the window
	// had token volume with no price, so even the direct figure is a lower bound.
	WastedUSD        float64
	WastedIncomplete bool
	// Attention is the ranked shortlist of sessions worth a look, worst first: errored, then
	// abandoned, then failing grades, then the unusually expensive. It is bounded by the
	// caller's limit, so it is a triage queue rather than a full listing.
	Attention []AttentionRow
}

// CompletionRate is completed work as a share of the settled work items (0..100), the
// throughput figure the verdict strip leads with. It divides over settled work only, so
// in-flight sessions neither inflate nor depress it, and returns -1 when nothing has
// settled yet, which the view dashes rather than printing a 0 or 100 that would read as a
// real (and misleading) rate.
func (a AuditSummary) CompletionRate() float64 {
	if a.Settled == 0 {
		return -1
	}
	return float64(a.Completed) / float64(a.Settled) * 100
}

// GPA is the mean grade point over the graded work items on the A=4..F=0 scale, or -1
// when no session in scope carries a current grade. It matches the Insights GPA line's
// scale and gated cohort, so the two surfaces agree on the same window. The view dashes a
// -1 rather than printing 0, which would read as a fleet of straight F's.
func (a AuditSummary) GPA() float64 {
	if a.Graded == 0 {
		return -1
	}
	return a.GradePoints / float64(a.Graded)
}

// AttentionRow is one session on the audit shortlist: enough identity to link and label
// it, its cost and grade and outcome, and the canonical Reason key that put it on the
// list. Reason is the store's raw tier ("errored", "abandoned", "grade-f", "grade-d",
// "costly"); the view maps it to a human phrase, the same store-keeps-the-key split
// LabeledCount uses so the two layers stay decoupled.
type AttentionRow struct {
	ID             int64
	Agent          string
	ProjectKey     string
	ProjectName    string
	ProjectKind    string
	Title          string
	Grade          *string
	Outcome        string
	CostUSD        float64
	CostIncomplete bool
	MessageCount   int
	StartedAt      *time.Time
	Reason         string
}

// attentionLimit caps the audit shortlist. It is a triage queue a team lead skims top to
// bottom, not a full listing (the Sessions feed is that), so a handful of the worst runs
// is the whole point; a longer list would bury the signal the strip exists to surface.
const attentionLimit = 8

// The costly tier flags a run whose cost is costlyMultiple times the median non-zero cost
// in scope, but only once at least costlyMinCohort runs have spent anything, so "typical
// cost" rests on more than one or two samples. A multiple over the median (not a top-N
// percentile) degrades gracefully at every cohort size: a lone session is never its own
// outlier, and a fleet of near-equal runs flags none, while one run costing several times
// the norm still surfaces. It is a soft, supplementary signal: the outcome and grade tiers
// take precedence, so a costly run that also errored reads as errored, not costly.
const (
	costlyMultiple  = 3
	costlyMinCohort = 4
)

// OverviewAudit computes the audit verdict, its wasted-spend figure, and the needs-attention
// shortlist for the Overview, all scoped by f. It runs the three reads in one repeatable-read,
// read-only snapshot so they describe the same cohort: a session insert or a signals_stale flip
// between the queries could otherwise let the shortlist name a run the verdict never counted, or
// let the wasted-spend sum price rows the count did not see. The snapshot takes no row locks, so
// it never blocks ingest, matching QualityDistribution's standalone snapshot.
func (s *Store) OverviewAudit(ctx context.Context, f AnalyticsFilter) (AuditSummary, error) {
	var out AuditSummary
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = s.overviewAuditFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return AuditSummary{}, fmt.Errorf("overview audit snapshot: %w", err)
	}
	return out, nil
}

// overviewAuditFrom assembles the audit summary from one querier: the verdict counts, the
// wasted-spend figure, and the attention shortlist. Taking a querier lets the standalone
// OverviewAudit and the combined OverviewData share it, so the three reads always run against
// one caller's snapshot rather than three independently-timed ones.
func (s *Store) overviewAuditFrom(ctx context.Context, q querier, f AnalyticsFilter) (AuditSummary, error) {
	out, err := s.overviewVerdict(ctx, q, f)
	if err != nil {
		return AuditSummary{}, err
	}
	if out.WastedUSD, out.WastedIncomplete, err = s.wastedSpend(ctx, q, f); err != nil {
		return AuditSummary{}, err
	}
	if out.Attention, err = s.attentionRows(ctx, q, f); err != nil {
		return AuditSummary{}, err
	}
	return out, nil
}

// OverviewData reads the Overview's usage analytics and audit verdict as one repeatable-read
// snapshot, so the Spend tile's total (Analytics.TotalCost) and the "on failed runs" figure the
// tile pulls out of it (AuditSummary.WastedUSD, a subset of that total by construction) are
// computed against one MVCC snapshot. Read in two separate transactions, an ingest or rebuild
// that commits a failed usage_event between the calls could make WastedUSD name spend the
// TotalCost it annotates has not counted, so the subfigure would exceed the aggregate it sits
// under. Both reads are read-only and take no locks, so this never blocks ingest, matching the
// standalone Analytics and OverviewAudit snapshots it replaces on the Overview path.
func (s *Store) OverviewData(ctx context.Context, f AnalyticsFilter) (Analytics, AuditSummary, error) {
	var a Analytics
	var au AuditSummary
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			if a, err = s.analyticsFrom(ctx, tx, f); err != nil {
				return err
			}
			au, err = s.overviewAuditFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return Analytics{}, AuditSummary{}, fmt.Errorf("overview snapshot: %w", err)
	}
	return a, au, nil
}

// overviewVerdict is the one-scan aggregate behind the verdict strip's counts: the work-item
// count, the settled/completed/wasted partition, and the graded cohort with its grade-point
// sum. It scopes over top-level sessions (relationship_type <> 'subagent') left-joined to
// their current signals, so an unsettled or stale-graded session still counts as a work item
// but folds into neither the settled nor the graded bucket, exactly as the Insights
// distributions treat it. The grade-point CASE uses the app's A=4..F=0 scale so the resulting
// GPA reconciles with the Insights GPA line. The wasted-spend figure is not computed here: it
// rides the usage_events occurred_at base rather than this session-started scan, so wastedSpend
// reads it separately and the Spend tile's subfigure stays a true slice of the Spend total (see
// AuditSummary.WastedUSD).
func (s *Store) overviewVerdict(ctx context.Context, q querier, f AnalyticsFilter) (AuditSummary, error) {
	filter, args := f.clauseFor("s.started_at")
	var out AuditSummary
	err := q.QueryRow(ctx, `
		SELECT
		  count(*),
		  count(*) FILTER (WHERE sig.outcome IN ('completed', 'errored', 'abandoned')),
		  count(*) FILTER (WHERE sig.outcome = 'completed'),
		  count(*) FILTER (WHERE sig.outcome IN ('errored', 'abandoned')),
		  count(*) FILTER (WHERE sig.grade IS NOT NULL),
		  coalesce(sum(CASE sig.grade
		                 WHEN 'A' THEN 4 WHEN 'B' THEN 3 WHEN 'C' THEN 2
		                 WHEN 'D' THEN 1 WHEN 'F' THEN 0 END), 0)::float8
		  FROM sessions s
		  LEFT JOIN session_signals sig ON sig.session_id = s.id AND `+signalsCurrent()+`
		 WHERE s.relationship_type <> 'subagent'`+filter, args...).
		Scan(&out.WorkItems, &out.Settled, &out.Completed, &out.Wasted,
			&out.Graded, &out.GradePoints)
	if err != nil {
		return AuditSummary{}, fmt.Errorf("overview verdict: %w", err)
	}
	return out, nil
}

// wastedSpend is the money the failed top-level runs burned in the window, the "on failed
// runs" figure the Spend tile pulls out of its total. It reads the same base the Spend total
// does (usage_events priced by cost_usd and dated by occurred_at, under the same scope and
// window from f.clause), then narrows to events whose session errored or was abandoned and is
// a top-level work item. It shares that base deliberately: this only adds restricting
// predicates to the sum behind Analytics.TotalCost, so the result is always a subset of it and
// cannot drift above the aggregate it annotates, and a run that started before the window but
// spent inside it lands in both figures alike. The relationship_type <> 'subagent' predicate keeps
// it the root run's own direct spend (a subagent's events carry the subagent's session id); see
// AuditSummary.WastedUSD for why the audit keeps the direct figure. The second column mirrors
// Analytics.CostIncomplete: some failed run in the window carried token volume with no price,
// so the direct figure is itself a lower bound.
func (s *Store) wastedSpend(ctx context.Context, q querier, f AnalyticsFilter) (float64, bool, error) {
	filter, args := f.clause()
	var usd float64
	var incomplete bool
	err := q.QueryRow(ctx, `
		SELECT coalesce(sum(ue.cost_usd), 0),
		       coalesce(`+costIncompleteExpr+`, false)
		  FROM usage_events ue
		  JOIN sessions s ON s.id = ue.session_id
		  JOIN session_signals sig ON sig.session_id = s.id AND `+signalsCurrent()+`
		 WHERE ue.occurred_at IS NOT NULL
		   AND s.relationship_type <> 'subagent'
		   AND sig.outcome IN ('errored', 'abandoned')`+filter, args...).
		Scan(&usd, &incomplete)
	if err != nil {
		return 0, false, fmt.Errorf("wasted spend: %w", err)
	}
	return usd, incomplete, nil
}

// attentionRows ranks the top-level sessions worth review and returns the worst
// attentionLimit of them. A session earns a place by tier, worst first: errored, then
// abandoned, then a failing grade (F before D), then an unusually expensive run (see the
// costly-tier constants). Within a tier the costliest, then most recent, sorts first, so
// the reader spends attention where the money and the freshness are. A session in no tier
// never appears, so the list is empty when nothing in scope warrants a look rather than
// padded with clean runs.
//
// The cost threshold is relative to this scope's own median spend, so "expensive" means
// several times a typical run in this window and these users, not an absolute dollar line
// that would flag every session on a busy instance and none on a quiet one. The CostUSD it
// returns and the costly tier both read the session's own direct cost (s.total_cost_usd),
// the same direct-cost basis as the verdict's WastedUSD, not the feed's whole-work-item
// rollup; see AuditSummary.WastedUSD for why the audit keeps the direct figure.
//
// Ranking runs on the session and signal columns alone, and only the surviving
// attentionLimit rows join projects and the shared title lateral. So the first-message
// lookup and its regexp run for the handful of rows the strip shows, not for every session
// in the window: the per-request work is bounded by the shortlist, not by the corpus. The
// shortlisted rows still read the same title and grade the Sessions feed would show them
// under, since the join reuses the same title lateral and signals gate.
func (s *Store) attentionRows(ctx context.Context, q querier, f AnalyticsFilter) ([]AttentionRow, error) {
	filter, args := f.clauseFor("s.started_at")
	multArg := len(args) + 1
	cohortArg := len(args) + 2
	limitArg := len(args) + 3
	args = append(args, costlyMultiple, costlyMinCohort, attentionLimit)
	rows, err := q.Query(ctx, `
		WITH scoped AS (
		  SELECT s.id, s.agent, s.project_id, s.total_cost_usd, s.cost_incomplete,
		         s.message_count, s.started_at, sig.grade, sig.outcome
		    FROM sessions s
		    LEFT JOIN session_signals sig ON sig.session_id = s.id AND `+signalsCurrent()+`
		   WHERE s.relationship_type <> 'subagent'`+filter+`
		),
		thr AS (
		  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY total_cost_usd) AS med,
		         count(*) AS n_priced
		    FROM scoped WHERE total_cost_usd > 0
		),
		ranked AS (
		  SELECT sc.*,
		         (thr.n_priced >= $`+itoa(cohortArg)+` AND thr.med > 0
		            AND sc.total_cost_usd >= $`+itoa(multArg)+` * thr.med) AS is_costly
		    FROM scoped sc CROSS JOIN thr
		),
		top AS (
		  SELECT id, agent, project_id, total_cost_usd, cost_incomplete, message_count,
		         started_at, grade, outcome,
		         CASE
		           WHEN outcome = 'errored'   THEN 5
		           WHEN outcome = 'abandoned' THEN 4
		           WHEN grade = 'F'           THEN 3
		           WHEN grade = 'D'           THEN 2
		           WHEN is_costly             THEN 1
		           ELSE 0
		         END AS tier,
		         CASE
		           WHEN outcome = 'errored'   THEN 'errored'
		           WHEN outcome = 'abandoned' THEN 'abandoned'
		           WHEN grade = 'F'           THEN 'grade-f'
		           WHEN grade = 'D'           THEN 'grade-d'
		           WHEN is_costly             THEN 'costly'
		           ELSE ''
		         END AS reason
		    FROM ranked
		   WHERE outcome IN ('errored', 'abandoned') OR grade IN ('D', 'F') OR is_costly
		   ORDER BY tier DESC, total_cost_usd DESC, started_at DESC NULLS LAST
		   LIMIT $`+itoa(limitArg)+`
		)
		SELECT s.id, s.agent, p.remote_key, p.display_name, p.kind, coalesce(title.content, ''),
		       s.grade, s.outcome, s.total_cost_usd, s.cost_incomplete, s.message_count,
		       s.started_at, s.reason
		  FROM top s
		  JOIN projects p ON p.id = s.project_id
		  `+titleLateralSQL+`
		 ORDER BY s.tier DESC, s.total_cost_usd DESC, s.started_at DESC NULLS LAST`, args...)
	if err != nil {
		return nil, fmt.Errorf("query attention rows: %w", err)
	}
	defer rows.Close()

	var out []AttentionRow
	for rows.Next() {
		var r AttentionRow
		var outcome *string
		if err := rows.Scan(&r.ID, &r.Agent, &r.ProjectKey, &r.ProjectName, &r.ProjectKind,
			&r.Title, &r.Grade, &outcome, &r.CostUSD, &r.CostIncomplete,
			&r.MessageCount, &r.StartedAt, &r.Reason); err != nil {
			return nil, fmt.Errorf("scan attention row: %w", err)
		}
		if outcome != nil {
			r.Outcome = *outcome
		}
		r.Title = squashSpaces(r.Title)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attention rows: %w", err)
	}
	return out, nil
}
