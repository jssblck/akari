package store_test

import (
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// outcomeCount reads one outcome bucket out of a quality distribution, for asserting how a
// split placed a session.
func outcomeCount(qd store.QualityDistribution, key string) int {
	for _, b := range qd.Outcomes {
		if b.Key == key {
			return b.Count
		}
	}
	return 0
}

// TestSessionSignalsByIDStaleAfterAppendReadsUnmeasured pins the read-side freshness gate on the
// session header. Once a settled session is graded, the header reads its stored outcome; but a
// later appended region moves the projection past that grade, so the stored row now describes an
// earlier, smaller session. The header must read it as unmeasured (a neutral unknown/unscored
// result) until the settle pass re-grades it, rather than show a grade for a session that has
// since grown. This is the read mirror of the signals_stale flag the settle pass drains on.
func TestSessionSignalsByIDStaleAfterAppendReadsUnmeasured(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-stale-read", 120)

	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("grade: %v", err)
	} else if n != 1 {
		t.Fatalf("grade count = %d, want 1", n)
	}
	if sig, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("read graded: %v", err)
	} else if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Fatalf("graded outcome = %s, want abandoned", sig.Outcome)
	}

	// A post-settle append re-marks the session stale, the way applyAggregates does on a real
	// appended region. The read gate keys on the flag, so setting it alone must make the grade
	// read as unmeasured.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET signals_stale = true WHERE id = $1", sid); err != nil {
		t.Fatalf("simulate post-settle append: %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read stale: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeUnknown) || sig.Scored() {
		t.Errorf("stale read = (%s, scored %v), want neutral (unknown, unscored)", sig.Outcome, sig.Scored())
	}

	// Re-grading restores a usable read.
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("re-grade: %v", err)
	}
	if sig, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("read re-graded: %v", err)
	} else if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("re-graded outcome = %s, want abandoned", sig.Outcome)
	}
}

// TestMetadataBumpKeepsGradeReadable pins why the read gate keys on signals_stale rather than a
// refreshed_at >= updated_at comparison. A metadata-only write (an announce re-announce, a
// devseed owner reassignment) bumps sessions.updated_at without moving the projection or setting
// the flag. A timestamp gate would then read the grade as stale and, because the settle pass
// keys on the flag, never revisit it, stranding the grade unread forever. The flag gate leaves
// the grade readable, on the header and in the fleet aggregates alike.
func TestMetadataBumpKeepsGradeReadable(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-meta-bump", 120)
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("grade: %v", err)
	}

	// A metadata-only write bumps updated_at but does not touch the projection or the flag,
	// exactly what announceIntoProjectTx and the devseed reassignment do.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET updated_at = now() WHERE id = $1", sid); err != nil {
		t.Fatalf("metadata bump: %v", err)
	}

	if sig, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("read after metadata bump: %v", err)
	} else if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("header outcome after metadata bump = %s, want abandoned (a rename must not strand the grade)", sig.Outcome)
	}
	qd, err := st.QualityDistribution(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("quality: %v", err)
	}
	if got := outcomeCount(qd, string(quality.OutcomeAbandoned)); got != 1 {
		t.Errorf("abandoned after metadata bump = %d, want 1 (the grade still counts)", got)
	}
}

// TestInsightsAggregatesExcludeStaleSignals pins the same freshness gate on the fleet aggregates.
// A session whose grade went stale after a post-settle append must not carry its old grade into
// the cohort figures: the quality split folds it into the unscored/unknown miss bucket (its
// total still covers every scoped session, so it reconciles with the archetype split), and the
// measured cohorts (hygiene, context) drop it entirely until it is re-graded.
func TestInsightsAggregatesExcludeStaleSignals(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	seedSettledSession(t, st, ctx, uid, pid, "agg-a", 120)
	b := seedSettledSession(t, st, ctx, uid, pid, "agg-b", 121)
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("grade both: %v", err)
	}
	f := store.AnalyticsFilter{ProjectID: pid}

	qd, err := st.QualityDistribution(ctx, f)
	if err != nil {
		t.Fatalf("quality: %v", err)
	}
	if qd.Sessions != 2 || outcomeCount(qd, string(quality.OutcomeAbandoned)) != 2 {
		t.Fatalf("before stale: sessions %d, abandoned %d, want 2 and 2", qd.Sessions, outcomeCount(qd, string(quality.OutcomeAbandoned)))
	}
	if hy, err := st.PromptHygiene(ctx, f); err != nil {
		t.Fatalf("hygiene: %v", err)
	} else if hy.Sessions != 2 {
		t.Fatalf("hygiene cohort before stale = %d, want 2", hy.Sessions)
	}

	// Post-settle append on b: applyAggregates would set signals_stale, so set it here.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET signals_stale = true WHERE id = $1", b); err != nil {
		t.Fatalf("stale b: %v", err)
	}

	qd2, err := st.QualityDistribution(ctx, f)
	if err != nil {
		t.Fatalf("quality2: %v", err)
	}
	if qd2.Sessions != 2 {
		t.Errorf("quality sessions after stale = %d, want 2 (still counts every scoped session)", qd2.Sessions)
	}
	if got := outcomeCount(qd2, string(quality.OutcomeAbandoned)); got != 1 {
		t.Errorf("abandoned after stale = %d, want 1 (only the fresh session)", got)
	}
	if got := outcomeCount(qd2, string(quality.OutcomeUnknown)); got != 1 {
		t.Errorf("unknown after stale = %d, want 1 (the stale session folds here)", got)
	}
	if hy2, err := st.PromptHygiene(ctx, f); err != nil {
		t.Fatalf("hygiene2: %v", err)
	} else if hy2.Sessions != 1 {
		t.Errorf("hygiene cohort after stale = %d, want 1 (the stale session drops from the measured cohort)", hy2.Sessions)
	}
}

// TestInsightsCohortTotalsReconcile pins the cross-panel reconciliation the one-snapshot read
// guarantees: the quality split's session total, the outcome split's total, and the archetype
// split's total all cover the same scoped session set, so read from one MVCC snapshot they agree
// exactly. A page that read them on separate connections could disagree by a session that landed
// mid-render.
func TestInsightsCohortTotalsReconcile(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	for i, src := range []string{"ins-a", "ins-b", "ins-c"} {
		seedSettledSession(t, st, ctx, uid, pid, src, 120+i)
	}
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("grade: %v", err)
	}

	ins, err := st.Insights(ctx, store.AnalyticsFilter{ProjectID: pid}, store.AllInsightsPanels)
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	archTotal := 0
	for _, bkt := range ins.Archetypes {
		archTotal += bkt.Count
	}
	outcomeTotal := 0
	for _, bkt := range ins.Quality.Outcomes {
		outcomeTotal += bkt.Count
	}
	if ins.Quality.Sessions != 3 || archTotal != 3 || outcomeTotal != 3 {
		t.Errorf("cohort totals disagree: quality %d, archetypes %d, outcomes %d, want 3 each",
			ins.Quality.Sessions, archTotal, outcomeTotal)
	}
}
