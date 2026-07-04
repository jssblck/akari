package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// insertScoredSignals writes a graded session_signals row carrying a score and letter grade,
// then sets the session's signals_stale flag. It stands in for a settled, graded session:
// stale=false is the state the settle pass leaves a current grade (the gated reads count it),
// while stale=true stands in for a grade whose projection has since moved, which the gate must
// exclude. It is the scored counterpart of settle_test.go's insertSignalsRow.
func insertScoredSignals(t *testing.T, st *store.Store, ctx context.Context, sid int64, score int, grade string, stale bool) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade, refreshed_at)
		 VALUES ($1, 'completed', 'high', $2, $3, now())`,
		sid, score, grade); err != nil {
		t.Fatalf("insert scored signals: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = $2 WHERE id = $1`, sid, stale); err != nil {
		t.Fatalf("set signals_stale: %v", err)
	}
}

// TestSessionSignalsScoreGradeConsistency pins the migration 0040 invariants the card cohorts
// rest on: score and grade are a matched pair (one set without the other is rejected), and a set
// grade must equal GradeFor(score) (a grade that contradicts its score's band is rejected). This
// is why AvgQualityScore's grade-IS-NOT-NULL cohort provably carries a score to average, and why
// the card's letter-of-mean-score reconciles with the panel's stored grades.
func TestSessionSignalsScoreGradeConsistency(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	scoreOnly := seedSession(t, st, uid, pid, "score-only")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, refreshed_at)
		 VALUES ($1, 'completed', 'high', 88, now())`, scoreOnly); err == nil {
		t.Fatal("a score without a grade should violate the coupling constraint, but the insert succeeded")
	}

	gradeOnly := seedSession(t, st, uid, pid, "grade-only")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, grade, refreshed_at)
		 VALUES ($1, 'completed', 'high', 'B', now())`, gradeOnly); err == nil {
		t.Fatal("a grade without a score should violate the coupling constraint, but the insert succeeded")
	}

	// A grade that contradicts its score's band (95 bands to A, not F) is rejected too, so a
	// row's stored grade can never disagree with what its score would grade to.
	mismatch := seedSession(t, st, uid, pid, "band-mismatch")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade, refreshed_at)
		 VALUES ($1, 'completed', 'high', 95, 'F', now())`, mismatch); err == nil {
		t.Fatal("a grade that does not match GradeFor(score) should violate the band constraint, but the insert succeeded")
	}

	// Both NULL (an unscored session) and a band-consistent pair (a scored one) are the allowed shapes.
	unscored := seedSession(t, st, uid, pid, "unscored")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, refreshed_at)
		 VALUES ($1, 'unknown', 'low', now())`, unscored); err != nil {
		t.Fatalf("an unscored row (both NULL) should be allowed: %v", err)
	}
	scored := seedSession(t, st, uid, pid, "scored")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade, refreshed_at)
		 VALUES ($1, 'completed', 'high', 82, 'B', now())`, scored); err != nil {
		t.Fatalf("a band-consistent scored row (82 -> B) should be allowed: %v", err)
	}
}

// TestAvgQualityScore pins the mean the project OG card rounds into its QUALITY letter: it
// averages only the sessions carrying a gated (fresh) grade, so a stale-flagged grade is
// excluded, and it returns nil (not zero) when no session in scope is scored, the
// "unmeasured" default the card dashes.
func TestAvgQualityScore(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	// No graded session yet: the mean is unmeasured, so the card dashes rather than
	// printing a zero that would read as a failing average.
	avg, err := st.AvgQualityScore(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("avg with no grades: %v", err)
	}
	if avg != nil {
		t.Fatalf("avg with no grades = %v, want nil (unmeasured)", *avg)
	}

	// Two gated grades (80 and 90) average to 85; the gate must fold in only these.
	insertScoredSignals(t, st, ctx, seedSession(t, st, uid, pid, "graded-80"), 80, "B", false)
	insertScoredSignals(t, st, ctx, seedSession(t, st, uid, pid, "graded-90"), 90, "A", false)
	// A stale-flagged grade (its projection moved since it was graded) is excluded, so it
	// cannot drag the mean.
	insertScoredSignals(t, st, ctx, seedSession(t, st, uid, pid, "stale-0"), 0, "F", true)

	avg, err = st.AvgQualityScore(ctx, store.AnalyticsFilter{ProjectID: pid})
	if err != nil {
		t.Fatalf("avg with grades: %v", err)
	}
	if avg == nil {
		t.Fatal("avg with grades = nil, want the mean of the two gated grades")
	}
	if math.Abs(*avg-85) > 1e-9 {
		t.Fatalf("avg = %v, want 85 (mean of 80 and 90, the two gated grades only)", *avg)
	}
	// The card rounds the mean to a letter: 85 bands to a B on the standard thresholds.
	if g := quality.GradeFor(int(math.Round(*avg))); g != "B" {
		t.Fatalf("GradeFor(round(%v)) = %q, want %q", *avg, g, "B")
	}

	// A project scope with no sessions is unmeasured, so a different project's card cannot
	// borrow this project's grades.
	other, err := st.UpsertProject(ctx, "github.com/gracehopper/nanosecond", "github.com", "gracehopper", "nanosecond", "nanosecond", "remote")
	if err != nil {
		t.Fatalf("upsert other project: %v", err)
	}
	if avg, err := st.AvgQualityScore(ctx, store.AnalyticsFilter{ProjectID: other}); err != nil || avg != nil {
		t.Fatalf("avg for empty project = (%v, %v), want (nil, nil)", avg, err)
	}
}

// TestProjectCardSnapshotReconcilesAnalyticsAndQuality pins the guarantee the project card
// depends on: the analytics totals and the quality average come back from one snapshot, and
// that average equals the standalone read over the same filter (the two run the identical
// query, now just off one MVCC snapshot instead of two pooled connections that could straddle
// a rebuild). ok is true when every session in scope is already on the current parser epoch.
func TestProjectCardSnapshotReconcilesAnalyticsAndQuality(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	f := store.AnalyticsFilter{ProjectID: pid, OmitUsers: true}

	s1 := seedSessionWithStats(t, st, uid, pid, "claude", "s1", 1.0, 100, 20)
	s2 := seedSessionWithStats(t, st, uid, pid, "claude", "s2", 1.0, 200, 40)
	seedUsage(t, st, s1, "claude-opus-4-8", 1.0, 100, 20, 1, "u1")
	seedUsage(t, st, s2, "claude-opus-4-8", 1.0, 200, 40, 1, "u2")
	insertScoredSignals(t, st, ctx, s1, 70, "C", false)
	insertScoredSignals(t, st, ctx, s2, 90, "A", false)

	a, avg, ok, err := st.ProjectCardSnapshot(ctx, f)
	if err != nil || !ok {
		t.Fatalf("snapshot = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	if a.Sessions != 2 {
		t.Fatalf("snapshot sessions = %d, want 2 (both sessions have usage in scope)", a.Sessions)
	}
	if avg == nil {
		t.Fatal("snapshot avg = nil, want the mean of the two grades")
	}
	if math.Abs(*avg-80) > 1e-9 {
		t.Fatalf("snapshot avg = %v, want 80 (mean of 70 and 90)", *avg)
	}
	// The card rounds it to a B (80 bands to B), the same figure GenerateProject stamps.
	if g := quality.GradeFor(int(math.Round(*avg))); g != "B" {
		t.Fatalf("GradeFor(round(%v)) = %q, want B", *avg, g)
	}
	// The snapshot average matches the standalone pooled read over the same filter, so folding
	// the read into the snapshot changed only its consistency, not its value.
	standalone, err := st.AvgQualityScore(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if standalone == nil || math.Abs(*standalone-*avg) > 1e-9 {
		t.Fatalf("standalone avg %v != snapshot avg %v", standalone, *avg)
	}
}
