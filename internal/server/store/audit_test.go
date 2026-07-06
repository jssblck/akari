package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// setSessionCost stamps a session's rolled-up cost, which the audit's costly-tier percentile
// and each attention row's CostUSD read straight off the session row. The wasted-spend sum no
// longer reads this column: it prices from usage_events dated by occurred_at (see wastedSpend),
// so a test that asserts WastedUSD seeds usage_events for the failed runs as well.
func setSessionCost(t *testing.T, st *store.Store, ctx context.Context, sid int64, cost float64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET total_cost_usd = $2 WHERE id = $1`, sid, cost); err != nil {
		t.Fatalf("set cost for session %d: %v", sid, err)
	}
}

// markSubagent demotes a session to a subagent so the audit must exclude it: a team lead
// audits work items, not the fan-out under them, so a subagent counts toward nothing here
// even when it is the worst-behaved row in the project.
func markSubagent(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET relationship_type = 'subagent' WHERE id = $1`, sid); err != nil {
		t.Fatalf("mark subagent for session %d: %v", sid, err)
	}
}

// TestOverviewAudit pins the Overview audit read against a hand-built cohort: the verdict
// counts partition the top-level work items exactly (a live session counts as work but not
// as settled; a subagent counts as nothing), the GPA and wasted-spend reconcile with the
// same gated grades and outcomes the Insights panels use, and the attention shortlist ranks
// worst-first by tier (errored, abandoned, failing grade, then unusually expensive) with the
// subagent never surfacing.
func TestOverviewAudit(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	g := func(s string) *string { return &s }

	completedA := seedSession(t, st, uid, pid, "completed-a")
	insertGradeOutcomeSignal(t, st, ctx, completedA, g("A"), "completed")
	setSessionCost(t, st, ctx, completedA, 1.00)

	completedB := seedSession(t, st, uid, pid, "completed-b")
	insertGradeOutcomeSignal(t, st, ctx, completedB, g("B"), "completed")
	setSessionCost(t, st, ctx, completedB, 2.00)

	errored := seedSession(t, st, uid, pid, "errored")
	insertGradeOutcomeSignal(t, st, ctx, errored, g("F"), "errored")
	setSessionCost(t, st, ctx, errored, 5.00)
	// The wasted-spend sum prices the failed runs from their dated usage_events, so seed the
	// errored and abandoned runs' spend on the time axis; the session row's cost still feeds the
	// costly-tier percentile and the attention CostUSD.
	seedUsage(t, st, errored, "claude-opus-4-8", 5.00, 1000, 500, 1, "audit-errored-ue")

	abandoned := seedSession(t, st, uid, pid, "abandoned")
	insertGradeOutcomeSignal(t, st, ctx, abandoned, nil, "abandoned")
	setSessionCost(t, st, ctx, abandoned, 3.00)
	seedUsage(t, st, abandoned, "claude-opus-4-8", 3.00, 800, 400, 1, "audit-abandoned-ue")

	gradedD := seedSession(t, st, uid, pid, "graded-d")
	insertGradeOutcomeSignal(t, st, ctx, gradedD, g("D"), "completed")
	setSessionCost(t, st, ctx, gradedD, 0.50)

	// A live session (no signals row) counts as a work item but has not settled, so it drags
	// neither the completion rate nor the GPA and never earns an attention flag.
	live := seedSession(t, st, uid, pid, "live")
	setSessionCost(t, st, ctx, live, 0.10)

	// The costliest run completed cleanly with a top grade, so it earns its place on the
	// shortlist only through the costly tier (its cost sits above the scope's 90th percentile).
	costly := seedSession(t, st, uid, pid, "costly")
	insertGradeOutcomeSignal(t, st, ctx, costly, g("A"), "completed")
	setSessionCost(t, st, ctx, costly, 20.00)

	// The worst-behaved row in the project is a subagent: errored, failing, and expensive. The
	// audit must ignore it entirely, in both the verdict counts and the shortlist.
	sub := seedSession(t, st, uid, pid, "subagent")
	insertGradeOutcomeSignal(t, st, ctx, sub, g("F"), "errored")
	setSessionCost(t, st, ctx, sub, 9.00)
	markSubagent(t, st, ctx, sub)

	au, err := st.OverviewAudit(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("overview audit: %v", err)
	}

	if au.WorkItems != 7 {
		t.Errorf("WorkItems = %d, want 7 (all top-level, subagent excluded)", au.WorkItems)
	}
	if au.Settled != 6 {
		t.Errorf("Settled = %d, want 6 (the live session has not settled)", au.Settled)
	}
	if au.Completed != 4 {
		t.Errorf("Completed = %d, want 4", au.Completed)
	}
	if au.Wasted != 2 {
		t.Errorf("Wasted = %d, want 2 (errored + abandoned)", au.Wasted)
	}
	if au.Graded != 5 {
		t.Errorf("Graded = %d, want 5 (abandoned is ungraded, live has no row)", au.Graded)
	}
	// GPA over A(4), B(3), F(0), D(1), A(4) = 12 points over 5 graded = 2.4.
	if gpa := au.GPA(); math.Abs(gpa-2.4) > 1e-9 {
		t.Errorf("GPA() = %v, want 2.4", gpa)
	}
	// Completion is 4 completed over 6 settled, not over 7 work items: the live session must
	// not count against the rate as if it had failed.
	if rate := au.CompletionRate(); math.Abs(rate-(4.0/6.0*100)) > 1e-9 {
		t.Errorf("CompletionRate() = %v, want %v", rate, 4.0/6.0*100)
	}
	// Wasted spend is the errored ($5) plus abandoned ($3) top-level runs' dated usage; the
	// subagent's $9 and the completed runs' cost are not waste. The completed and costly runs
	// carry no usage_events here, so only the two failed runs contribute to the sum.
	if math.Abs(au.WastedUSD-8.00) > 1e-9 {
		t.Errorf("WastedUSD = %v, want 8.00", au.WastedUSD)
	}

	// The shortlist ranks worst-first by tier: errored, abandoned, the failing grade, then the
	// costly run. The subagent (errored, F, $9) never appears.
	wantOrder := []struct {
		id     int64
		reason string
	}{
		{errored, "errored"},
		{abandoned, "abandoned"},
		{gradedD, "grade-d"},
		{costly, "costly"},
	}
	if len(au.Attention) != len(wantOrder) {
		t.Fatalf("Attention has %d rows, want %d: %+v", len(au.Attention), len(wantOrder), au.Attention)
	}
	for i, want := range wantOrder {
		got := au.Attention[i]
		if got.ID != want.id || got.Reason != want.reason {
			t.Errorf("Attention[%d] = {id %d, reason %q}, want {id %d, reason %q}",
				i, got.ID, got.Reason, want.id, want.reason)
		}
		if got.ID == sub {
			t.Errorf("Attention[%d] is the subagent, which must never surface", i)
		}
	}
}

// linkSubagent makes child a subagent of parent (parent_session_id plus the relationship
// tag), so the child's own spend belongs to the parent's whole-work-item rollup on the feed
// but must never fold into the audit's direct verdict or attention costs.
func linkSubagent(t *testing.T, st *store.Store, ctx context.Context, parent, child int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET parent_session_id = $1, relationship_type = 'subagent' WHERE id = $2`,
		parent, child); err != nil {
		t.Fatalf("link subagent %d under %d: %v", child, parent, err)
	}
}

// TestOverviewAuditCostsAreDirect pins the audit's direct-cost basis: WastedUSD and an
// attention row's CostUSD count a failed top-level run's OWN cost, never the cost of the
// subagent subtree it fanned out. The feed's fan-out chip carries the whole-work-item figure
// (root plus subtree, see TreeRollup); the Overview deliberately keeps the direct cost, so a
// $2 errored root that spawned a $50 subagent reads as $2 of waste here, not $52.
func TestOverviewAuditCostsAreDirect(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	g := func(s string) *string { return &s }

	root := seedSession(t, st, uid, pid, "errored-root")
	insertGradeOutcomeSignal(t, st, ctx, root, g("F"), "errored")
	setSessionCost(t, st, ctx, root, 2.00)
	// The wasted-spend sum prices from dated usage_events, so seed the root's own $2 and the
	// subagent's $50 on the time axis. wastedSpend keeps only the top-level root's events, so
	// the subtree's $50 must not fold into the direct figure.
	seedUsage(t, st, root, "claude-opus-4-8", 2.00, 500, 250, 1, "direct-root-ue")

	// An expensive subagent under that failed root. Its spend is part of the work item's
	// whole-tree cost, but the audit excludes subagents and counts only the root's own cost.
	sub := seedSession(t, st, uid, pid, "expensive-subagent")
	setSessionCost(t, st, ctx, sub, 50.00)
	seedUsage(t, st, sub, "claude-opus-4-8", 50.00, 20000, 10000, 1, "direct-sub-ue")
	linkSubagent(t, st, ctx, root, sub)

	au, err := st.OverviewAudit(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("overview audit: %v", err)
	}
	if math.Abs(au.WastedUSD-2.00) > 1e-9 {
		t.Errorf("WastedUSD = %v, want 2.00 (the root's own cost, not the $50 subagent subtree)", au.WastedUSD)
	}
	if len(au.Attention) != 1 {
		t.Fatalf("Attention has %d rows, want 1 (the errored root; the subagent is excluded): %+v", len(au.Attention), au.Attention)
	}
	got := au.Attention[0]
	if got.ID != root {
		t.Errorf("Attention[0].ID = %d, want the errored root %d", got.ID, root)
	}
	if math.Abs(got.CostUSD-2.00) > 1e-9 {
		t.Errorf("Attention[0].CostUSD = %v, want 2.00 (direct root cost, not the whole-tree $52)", got.CostUSD)
	}
}

// TestOverviewAuditWastedSpendTracksOccurredAt pins the fix for the Spend tile's basis
// mismatch: WastedUSD shares the Spend total's usage_events occurred_at window, not the
// session-started window the verdict counts use. A run that started before the window but
// burned tokens inside it is money the window spent, so the Spend total counts it and the "on
// failed runs" subfigure has to count it too, or the subfigure would claim to be a slice of a
// total it sits outside. The run is not a work item of this window (it did not start here), yet
// its in-window failed spend still lands in WastedUSD; on the old session-started basis it was
// silently dropped.
func TestOverviewAuditWastedSpendTracksOccurredAt(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	// A run that started well before the window and was abandoned, but whose usage landed
	// inside the window.
	old := seedSession(t, st, uid, pid, "started-before-window")
	insertGradeOutcomeSignal(t, st, ctx, old, nil, "abandoned")
	setSessionCost(t, st, ctx, old, 4.00)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET started_at = now() - make_interval(days => 10) WHERE id = $1`, old); err != nil {
		t.Fatalf("backdate session start: %v", err)
	}
	seedUsage(t, st, old, "claude-opus-4-8", 4.00, 900, 450, 1, "occurred-in-window-ue")

	// A window that opens after the run started but before its usage occurred.
	f := store.AnalyticsFilter{ProjectID: pid, Since: time.Now().Add(-3 * 24 * time.Hour)}
	au, err := st.OverviewAudit(ctx, f)
	if err != nil {
		t.Fatalf("overview audit: %v", err)
	}

	// The run started before the window, so it is not one of the window's work items.
	if au.WorkItems != 0 {
		t.Errorf("WorkItems = %d, want 0 (the run started before the window opened)", au.WorkItems)
	}
	// But its failed spend occurred inside the window, so it is wasted spend of this window, the
	// same usage the Spend total counts. On the old session-started basis this would have been 0.
	if math.Abs(au.WastedUSD-4.00) > 1e-9 {
		t.Errorf("WastedUSD = %v, want 4.00 (in-window usage of a run that started earlier)", au.WastedUSD)
	}
}

// TestOverviewDataWastedSubsetOfSpend pins that the combined Overview read returns the Spend
// total and the failed-run spend from one snapshot with the subset relation intact: WastedUSD
// (the "on failed runs" subfigure) never exceeds TotalCost (the tile's total), because both sum
// the same window's usage_events and WastedUSD only adds the failed and top-level predicates.
func TestOverviewDataWastedSubsetOfSpend(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	g := func(s string) *string { return &s }

	comp := seedSession(t, st, uid, pid, "od-completed")
	insertGradeOutcomeSignal(t, st, ctx, comp, g("A"), "completed")
	seedUsage(t, st, comp, "claude-opus-4-8", 6.00, 1000, 500, 1, "od-comp")

	errd := seedSession(t, st, uid, pid, "od-errored")
	insertGradeOutcomeSignal(t, st, ctx, errd, g("F"), "errored")
	seedUsage(t, st, errd, "claude-opus-4-8", 2.00, 400, 200, 1, "od-err")

	a, au, err := st.OverviewData(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("overview data: %v", err)
	}
	if au.WastedUSD > a.TotalCost+1e-9 {
		t.Errorf("WastedUSD %v exceeds TotalCost %v; the subfigure must be a subset of the total", au.WastedUSD, a.TotalCost)
	}
	if math.Abs(au.WastedUSD-2.00) > 1e-9 {
		t.Errorf("WastedUSD = %v, want 2.00 (the errored run's priced usage)", au.WastedUSD)
	}
	if math.Abs(a.TotalCost-8.00) > 1e-9 {
		t.Errorf("TotalCost = %v, want 8.00 (both runs' priced usage)", a.TotalCost)
	}
}

// TestOverviewAuditUnmeasured pins the unmeasured sentinels: a scope whose work has not
// settled or been graded reports -1 from CompletionRate and GPA (which the view dashes)
// rather than a 0 that would read as total failure, and its shortlist is empty rather than
// padded with the clean, in-flight run.
func TestOverviewAuditUnmeasured(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	live := seedSession(t, st, uid, pid, "live-only")
	setSessionCost(t, st, ctx, live, 0.25)

	au, err := st.OverviewAudit(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("overview audit: %v", err)
	}
	if au.WorkItems != 1 {
		t.Errorf("WorkItems = %d, want 1", au.WorkItems)
	}
	if au.Settled != 0 {
		t.Errorf("Settled = %d, want 0", au.Settled)
	}
	if rate := au.CompletionRate(); rate != -1 {
		t.Errorf("CompletionRate() = %v, want -1 (unmeasured)", rate)
	}
	if gpa := au.GPA(); gpa != -1 {
		t.Errorf("GPA() = %v, want -1 (unmeasured)", gpa)
	}
	if len(au.Attention) != 0 {
		t.Errorf("Attention = %+v, want empty (nothing settled or failing)", au.Attention)
	}
}
