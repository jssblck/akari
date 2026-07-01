package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// seedSettledSession seeds a user-last session (user, assistant, user) with its last
// activity endedMinsAgo in the past, the shape the classifier reads as abandoned once the
// gap clears the idle window. It is the fixture the settle-pass tests grade: past the
// window it settles to abandoned, so a materialized row is easy to pin.
func seedSettledSession(t *testing.T, st *store.Store, ctx context.Context, uid, pid int64, src string, endedMinsAgo int) int64 {
	t.Helper()
	sid := seedSession(t, st, uid, pid, src)
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
			{Ordinal: 2, Role: "user", Content: "one more thing"},
		},
	}); err != nil {
		t.Fatalf("apply delta for %s: %v", src, err)
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 2, ended_at = now() - make_interval(mins => $2) WHERE id = $1",
		sid, endedMinsAgo); err != nil {
		t.Fatalf("set session facts for %s: %v", src, err)
	}
	return sid
}

// insertSignalsRow writes a session_signals row directly, so a test can stand up a row as if
// an earlier pass (or a reparse taken while the session was still live) had computed it, with a
// chosen version and refreshed_at, then assert the settle pass revisits it. It also sets the
// session's signals_stale flag to the state a real grade would leave: a settled grade clears it
// (stale=false), a grade taken while the session was still live leaves it set (stale=true), so
// the caller says which case it is standing up. The settle pass drains on that flag, so the
// test's flag value is what makes the row due or not.
func insertSignalsRow(t *testing.T, st *store.Store, ctx context.Context, sid int64, version int, outcome, conf string, refreshedMinsAgo int, stale bool) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, refreshed_at)
		 VALUES ($1, $2, $3, $4, now() - make_interval(mins => $5))`,
		sid, version, outcome, conf, refreshedMinsAgo); err != nil {
		t.Fatalf("insert signals row: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET signals_stale = $2 WHERE id = $1`, sid, stale); err != nil {
		t.Fatalf("set signals_stale: %v", err)
	}
}

// TestRefreshSettledSignalsMaterializesSettledOnly is the core of the settle pass: it grades
// a session that has been idle past the abandoned window and leaves an unsettled one alone.
// A session that ended only minutes ago (still plausibly mid-conversation) and one that has
// not ended at all both stay ungraded, so the append path never has to compute signals and a
// live session is never stamped with a verdict that would drift as the idle gap grows.
func TestRefreshSettledSignalsMaterializesSettledOnly(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	settled := seedSettledSession(t, st, ctx, uid, pid, "sess-settled", 120)
	fresh := seedSettledSession(t, st, ctx, uid, pid, "sess-fresh", 5)
	live := seedSession(t, st, uid, pid, "sess-live") // never ended: ended_at stays NULL

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh settled: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d session(s), want 1 (only the settled one)", n)
	}
	if got := signalsRowCount(t, st, ctx, settled); got != 1 {
		t.Errorf("settled session signals row count = %d, want 1", got)
	}
	if got := signalsRowCount(t, st, ctx, fresh); got != 0 {
		t.Errorf("recently-ended session was graded (row count %d), want 0 until it settles", got)
	}
	if got := signalsRowCount(t, st, ctx, live); got != 0 {
		t.Errorf("still-live session was graded (row count %d), want 0", got)
	}
	sig, err := st.SessionSignalsByID(ctx, settled)
	if err != nil {
		t.Fatalf("read settled signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("settled outcome = %s, want abandoned", sig.Outcome)
	}
}

// TestRefreshSettledSignalsReStampsStaleVersion confirms the version reconcile: a settled
// session carrying a clean (signals_stale=false) row at a superseded signals_version is marked
// stale by reconcileStaleVersions and re-stamped, even though its projection never changed so
// the flag alone would not catch it. This is the path a quality.Version bump rides, the same
// version the read filter (SessionSignalsByID) and the Insights aggregates gate on.
func TestRefreshSettledSignalsReStampsStaleVersion(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-stale", 120)
	// A stale-version row that a prior settled grade left clean: signals_stale=false, so only
	// the version reconcile (not the projection-maintained flag) can make it due again. It is seeded
	// at quality.Version+1 because the session_signals_version_ck constraint forbids a version below 1
	// and the running quality.Version is 1, so the only seedable version that reads as stale (<> the
	// running version, which the reconcile re-stamps to current) is one above it.
	insertSignalsRow(t, st, ctx, sid, quality.Version+1, "completed", "high", 0, false)

	// Before the pass the current-version read finds no row and self-heals to unknown.
	if sig, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("read stale signals: %v", err)
	} else if sig.Outcome != string(quality.OutcomeUnknown) {
		t.Fatalf("stale-version read = %s, want unknown before re-stamp", sig.Outcome)
	}

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh settled: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d session(s), want 1 (the stale-version row)", n)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read re-stamped signals: %v", err)
	}
	if sig.Version != quality.Version || sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("re-stamped row = (version %d, %s), want (%d, abandoned)", sig.Version, sig.Outcome, quality.Version)
	}
}

// TestRefreshSettledSignalsReconcilesVersionOncePerBump pins the efficiency contract on the
// version reconcile: the signals_version <> current scan runs once per quality.Version change,
// gated on the parse_meta marker, not on every settle wake. A stale-version row present at the
// first pass is reconciled and drained; a second stale-version row that appears after the marker
// is current is left alone (a state production never reaches, since every fresh grade is written
// at the current version), and only resetting the marker (as a version bump would) makes the
// reconcile pick it up.
func TestRefreshSettledSignalsReconcilesVersionOncePerBump(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	a := seedSettledSession(t, st, ctx, uid, pid, "sess-ver-a", 120)
	insertSignalsRow(t, st, ctx, a, quality.Version+1, "completed", "high", 0, false)

	// First pass: the marker starts behind quality.Version, so the reconcile runs, marks the
	// stale-version row due, and the drain re-stamps it. The marker advances to the current version.
	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	} else if n != 1 {
		t.Fatalf("first pass refreshed %d, want 1 (the stale-version row)", n)
	}

	// A second stale-version row appears after the marker is current. The reconcile is gated, so
	// this pass must not scan it up: it stays at its stale version and reads as unknown.
	b := seedSettledSession(t, st, ctx, uid, pid, "sess-ver-b", 121)
	insertSignalsRow(t, st, ctx, b, quality.Version+1, "completed", "high", 0, false)
	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	} else if n != 0 {
		t.Errorf("second pass refreshed %d, want 0 (the reconcile is gated once per version)", n)
	}
	if sig, err := st.SessionSignalsByID(ctx, b); err != nil {
		t.Fatalf("read b: %v", err)
	} else if sig.Outcome != string(quality.OutcomeUnknown) {
		t.Errorf("b outcome = %s, want unknown (its stale-version row stays unreconciled)", sig.Outcome)
	}

	// Reset the marker, as a quality.Version bump leaves it, and the next pass reconciles b.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE parse_meta SET signals_reconciled_version = 0 WHERE id = TRUE"); err != nil {
		t.Fatalf("reset marker: %v", err)
	}
	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("post-bump pass: %v", err)
	} else if n != 1 {
		t.Errorf("post-bump pass refreshed %d, want 1 (b reconciled after the marker reset)", n)
	}
	if sig, err := st.SessionSignalsByID(ctx, b); err != nil {
		t.Fatalf("read b after bump: %v", err)
	} else if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("b outcome after bump = %s, want abandoned", sig.Outcome)
	}
}

// TestRefreshSettledSignalsMarkerIsMonotonic pins the rolling-deploy safety of the version reconcile.
// When an old (N-1) and a new (N) binary share one database, the new one advances the marker to N;
// the old one, waking afterward, must read N, find it at or ahead of its own version, and do nothing.
// It must NOT re-run reconcileStaleVersions (which at the old version would mark every N-version row
// stale and let the old scoring model overwrite it) and must NOT write the marker back down to N-1.
// This stands in the running binary for the OLD one by seeding a marker and a graded row a NEWER
// binary would have left (both a version ahead of quality.Version) and asserting the settle pass
// leaves the marker and the row untouched.
func TestRefreshSettledSignalsMarkerIsMonotonic(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	// A newer binary already reconciled at quality.Version+1 and graded this settled session at that
	// version, leaving it clean (signals_stale=false).
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-newer", 120)
	insertSignalsRow(t, st, ctx, sid, quality.Version+1, "abandoned", "medium", 0, false)
	if _, err := st.Pool.Exec(ctx,
		"UPDATE parse_meta SET signals_reconciled_version = $1 WHERE id = TRUE", quality.Version+1); err != nil {
		t.Fatalf("seed ahead marker: %v", err)
	}

	// The old binary's settle pass runs. The reconcile must skip (marker >= its version), so nothing
	// is marked due and nothing is re-graded.
	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("old-binary settle pass: %v", err)
	} else if n != 0 {
		t.Errorf("old-binary pass refreshed %d, want 0 (the reconcile must not run against a newer marker)", n)
	}

	// The marker must not have been stepped back to quality.Version.
	var marker int
	if err := st.Pool.QueryRow(ctx,
		"SELECT signals_reconciled_version FROM parse_meta WHERE id = TRUE").Scan(&marker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if marker != quality.Version+1 {
		t.Errorf("marker = %d after old-binary pass, want %d (never stepped back)", marker, quality.Version+1)
	}

	// The newer-version row must be untouched: still at its version and still clean, not re-marked
	// stale for the old scoring model to overwrite.
	var rowVersion int
	var stale bool
	if err := st.Pool.QueryRow(ctx,
		`SELECT sig.signals_version, s.signals_stale
		   FROM session_signals sig JOIN sessions s ON s.id = sig.session_id
		  WHERE sig.session_id = $1`, sid).Scan(&rowVersion, &stale); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if rowVersion != quality.Version+1 || stale {
		t.Errorf("newer row = (version %d, stale %v), want (%d, false) left untouched", rowVersion, stale, quality.Version+1)
	}
}

// TestRefreshSettledSignalsWriteGateSkipsSupersededGrade pins the per-session write gate, the deeper
// half of the rolling-deploy fix. Advancing the marker gates the reconcile, but an old settle pass
// that already selected a due session can still reach the per-session write; refreshSignalsTx rechecks
// the marker under the session lock and, seeing a newer binary has won, must leave the row and its
// signals_stale flag alone rather than overwrite the pending N grade with an N-1 one. The test seeds a
// due (signals_stale=true) session, advances the marker past the running version, and asserts the pass
// neither clears the flag nor changes the seeded row. It then rewinds the marker to the running version
// and reruns, proving the SAME session grades once no newer binary is ahead, so it is the gate, not
// some other skip, that held the write.
func TestRefreshSettledSignalsWriteGateSkipsSupersededGrade(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	// A settled session that is due (signals_stale=true) with a row at the running version. A grade
	// would flip it to abandoned and clear the flag, so those two are the tells that a write happened.
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-writegate", 120)
	insertSignalsRow(t, st, ctx, sid, quality.Version, "completed", "high", 0, true)

	// A newer binary won the marker.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE parse_meta SET signals_reconciled_version = $1 WHERE id = TRUE", quality.Version+1); err != nil {
		t.Fatalf("seed ahead marker: %v", err)
	}
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("settle pass with marker ahead: %v", err)
	}
	var stale bool
	var outcome string
	if err := st.Pool.QueryRow(ctx,
		`SELECT s.signals_stale, sig.outcome
		   FROM sessions s JOIN session_signals sig ON sig.session_id = s.id
		  WHERE s.id = $1`, sid).Scan(&stale, &outcome); err != nil {
		t.Fatalf("read row after gated pass: %v", err)
	}
	if !stale {
		t.Error("write gate should have left signals_stale=true; an old binary must not clear a superseded row")
	}
	if outcome != "completed" {
		t.Errorf("write gate should have left the seeded outcome intact, got %q (the grade must not have run)", outcome)
	}

	// Rewind the marker to the running version: no newer binary is ahead, so the same due session must
	// now grade and clear its flag. This proves the marker gate, not a promptFacts or settledness gate,
	// held the write above.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE parse_meta SET signals_reconciled_version = $1 WHERE id = TRUE", quality.Version); err != nil {
		t.Fatalf("rewind marker: %v", err)
	}
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("settle pass with marker current: %v", err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT s.signals_stale, sig.outcome
		   FROM sessions s JOIN session_signals sig ON sig.session_id = s.id
		  WHERE s.id = $1`, sid).Scan(&stale, &outcome); err != nil {
		t.Fatalf("read row after current pass: %v", err)
	}
	if stale {
		t.Error("with the marker current the due session should have graded and cleared signals_stale")
	}
	if outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("regraded outcome = %q, want abandoned (the settled fixture)", outcome)
	}
}

// TestRefreshSettledSignalsCorrectsPreSettleRow pins the correction the whole settle design
// exists to prevent: a grade taken before the session settled carries a not-yet-final outcome,
// and the settle pass must revisit it once the outcome stabilizes. refreshSignalsTx leaves
// signals_stale set whenever it grades a session that is not yet idle long enough (a reparse
// run while the session was still live), so the settle pass re-grades it once it settles. A
// second pass then finds nothing due, so the corrected outcome holds and does not oscillate.
func TestRefreshSettledSignalsCorrectsPreSettleRow(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	// Ended only minutes ago, so it is not yet settled: grading it now is the reparse-while-live
	// case.
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-presettle", 5)

	// Grade it while still live (as a reparse would). The outcome is not yet abandoned (not idle
	// long enough), and because the session is not settled the grade must leave signals_stale
	// set, so the settle pass will revisit it.
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("grade while live: %v", err)
	}
	if live, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("read live grade: %v", err)
	} else if live.Outcome != string(quality.OutcomeUnknown) {
		t.Fatalf("live grade outcome = %s, want unknown (not yet idle long enough)", live.Outcome)
	}

	// Time passes: the session's last activity is now well past the idle window. The settle pass
	// re-grades it because the pre-settle grade left it stale, and the stable outcome is abandoned.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET ended_at = now() - make_interval(mins => 120) WHERE id = $1", sid); err != nil {
		t.Fatalf("settle the session: %v", err)
	}

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh settled: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d session(s), want 1 (the pre-settle row)", n)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read corrected signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("pre-settle unknown not corrected: outcome = %s, want abandoned", sig.Outcome)
	}
	// The correction is stable: now settled and graded, the session's flag is cleared and it
	// drops out of the due set rather than being recomputed on every pass.
	n2, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("second refresh settled: %v", err)
	}
	if n2 != 0 {
		t.Errorf("settled session re-refreshed after settling: %d, want steady state (0)", n2)
	}
}

// TestRefreshSettledSignalsStableAcrossPasses states the projection-consistency invariant
// directly: once a settled session is graded, repeated settle passes neither re-refresh it
// nor move its stored outcome. This is the property the append path could not hold (a verdict
// taken mid-session drifted as the idle gap grew); computing it only once the session has
// settled makes it a fixed point.
func TestRefreshSettledSignalsStableAcrossPasses(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-stable", 120)

	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	} else if n != 1 {
		t.Fatalf("first refresh count = %d, want 1", n)
	}
	first, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read after first refresh: %v", err)
	}

	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	} else if n != 0 {
		t.Errorf("second refresh count = %d, want 0 (already settled and current)", n)
	}
	second, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read after second refresh: %v", err)
	}
	if first.Outcome != second.Outcome || first.Scored() != second.Scored() {
		t.Errorf("outcome drifted across passes: first (%s, scored %v), second (%s, scored %v)",
			first.Outcome, first.Scored(), second.Outcome, second.Scored())
	}
}

// TestRefreshSettledSignalsBatchDrains confirms one call drains a backlog larger than a
// single batch. With the batch size shrunk to two and three sessions due, the keyset cursor
// has to walk three internal batches in one call, refreshing every due session exactly once
// (a restart-per-batch scan would rescan the refreshed prefix each time). A second call
// finds the backlog drained and refreshes nothing.
func TestRefreshSettledSignalsBatchDrains(t *testing.T) {
	// Not parallel: SetSettledSignalBatch mutates a package global, so this must run in the
	// sequential phase rather than overlap the parallel settle tests that read it.
	defer store.SetSettledSignalBatch(2)()
	st, ctx, uid, pid := signalsEnv(t)
	for i, src := range []string{"drain-a", "drain-b", "drain-c"} {
		// Stagger ended_at so the oldest-first order is well defined; all are long settled.
		seedSettledSession(t, st, ctx, uid, pid, src, 120+i)
	}

	first, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if first != 3 {
		t.Errorf("first drain refreshed %d, want 3 (all due sessions across the internal batches)", first)
	}
	second, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if second != 0 {
		t.Errorf("second drain refreshed %d, want 0 (already drained)", second)
	}
}

// TestRefreshSettledSignalsReRefreshesOnLateProjectionChange covers the late-projection-change
// case. A historical transcript uploaded in several chunks keeps an ended_at far in the past,
// so a later chunk appends more turns without moving ended_at anywhere near now. If the settle
// pass graded the session between chunks, ended_at alone can never flag the row as due again,
// and it would reflect only the partial upload forever. applyAggregates sets signals_stale on
// every appended region, so the late chunk re-marks the session stale and the pass recomputes
// it, reflecting the added content. The test drives the append through the raw-delta seam and
// stamps the flag the way applyAggregates would.
func TestRefreshSettledSignalsReRefreshesOnLateProjectionChange(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-latechunk", 6000) // ended days ago

	if n, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if n != 1 {
		t.Fatalf("materialize count = %d, want 1", n)
	}
	before, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read after materialize: %v", err)
	}
	if before.PromptCount != 2 {
		t.Fatalf("prompt_count after first grade = %d, want 2", before.PromptCount)
	}

	// A later chunk of the same historical transcript appends another human turn. The append
	// path leaves signals untouched (that is the whole point), but stamps updated_at = now() and
	// re-marks signals_stale the way applyAggregates does; ended_at stays far in the past.
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 3, Role: "assistant", Content: "sure"},
			{Ordinal: 4, Role: "user", Content: "and one more real request please"},
		},
	}); err != nil {
		t.Fatalf("apply late chunk: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 3, updated_at = now(), signals_stale = true WHERE id = $1", sid); err != nil {
		t.Fatalf("bump updated_at for late chunk: %v", err)
	}

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh after late chunk: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d, want 1 (the late-chunk projection change)", n)
	}
	after, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read after late chunk: %v", err)
	}
	if after.PromptCount != 3 {
		t.Errorf("prompt_count after late chunk = %d, want 3 (the added human turn)", after.PromptCount)
	}
	// The correction is stable: the re-grade cleared signals_stale, so a further pass finds
	// nothing due.
	if n2, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("steady-state refresh: %v", err)
	} else if n2 != 0 {
		t.Errorf("re-refreshed after late chunk settled: %d, want 0", n2)
	}
}
