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

// insertSignalsRow writes a session_signals row directly, so a test can stand up a row as
// if an earlier pass (or a reparse taken while the session was still live) had computed it,
// with a chosen version and refreshed_at, then assert the settle pass revisits it.
func insertSignalsRow(t *testing.T, st *store.Store, ctx context.Context, sid int64, version int, outcome, conf string, refreshedMinsAgo int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, refreshed_at)
		 VALUES ($1, $2, $3, $4, now() - make_interval(mins => $5))`,
		sid, version, outcome, conf, refreshedMinsAgo); err != nil {
		t.Fatalf("insert signals row: %v", err)
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

// TestRefreshSettledSignalsReStampsStaleVersion confirms the version clause of the due
// predicate: a settled session carrying a row at a different signals_version is recomputed
// and re-stamped, even when that row was refreshed recently (so the refreshed_at clause does
// not fire). This is the path a quality.Version bump rides, the same one the read filter
// (SessionSignalsByID) and the Insights aggregates gate on.
func TestRefreshSettledSignalsReStampsStaleVersion(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-stale", 120)
	// A stale-version row refreshed just now: past both the settle point and the last
	// projection change, so neither the idle-window clause nor the source-changed clause
	// fires and only the version mismatch can make it due.
	insertSignalsRow(t, st, ctx, sid, quality.Version+1, "completed", "high", 0)

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

// TestRefreshSettledSignalsCorrectsPreSettleRow pins the refreshed_at clause, the one that
// fixes the drift the whole settle design exists to prevent. A row computed before the
// session settled (a reparse taken while it was live, or a resume that advanced ended_at
// past the old refresh) carries a not-yet-final outcome; because refreshed_at precedes
// ended_at + the idle window, the pass revisits it and recomputes against the stable idle
// gap. A second pass then finds nothing due, so the corrected outcome holds: it does not
// oscillate every wake.
func TestRefreshSettledSignalsCorrectsPreSettleRow(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-presettle", 120)
	// A row as it would stand if refreshed ten minutes after the session's last turn, well
	// inside the 30-minute window: the outcome then still reads unknown (not yet idle long
	// enough), and refreshed_at (110 minutes ago) precedes ended_at + idle (90 minutes ago).
	insertSignalsRow(t, st, ctx, sid, quality.Version, "unknown", "low", 110)
	// Pin the projection's last-change time before that refresh (115 minutes ago), so the
	// source-changed clause is off and only the idle-window clause can make this row due.
	// This isolates the correction that fires purely because time crossed the settle point,
	// with no new content: the reparse-graded-while-live case.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET updated_at = now() - make_interval(mins => 115) WHERE id = $1", sid); err != nil {
		t.Fatalf("pin updated_at: %v", err)
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
	// The correction is stable: now settled and freshly refreshed, the session drops out of
	// the due set rather than being recomputed on every pass.
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

// TestRefreshSettledSignalsReRefreshesOnLateProjectionChange covers the source-changed clause
// of the due predicate. A historical transcript uploaded in several chunks keeps an ended_at
// far in the past, so a later chunk appends more turns without moving ended_at anywhere near
// now. If the settle pass graded the session between chunks, ended_at alone can never flag the
// row as due again, and it would reflect only the partial upload forever. The clause that
// compares refreshed_at against the projection's updated_at (bumped on every appended region)
// catches exactly this: the row is recomputed once the late chunk lands, and reflects the
// added content.
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
	// path leaves signals untouched (that is the whole point), but stamps updated_at = now()
	// the way applyAggregates does; ended_at stays far in the past.
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 3, Role: "assistant", Content: "sure"},
			{Ordinal: 4, Role: "user", Content: "and one more real request please"},
		},
	}); err != nil {
		t.Fatalf("apply late chunk: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 3, updated_at = now() WHERE id = $1", sid); err != nil {
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
	// The correction is stable: refreshed_at now trails updated_at no longer, so a further
	// pass finds nothing due.
	if n2, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("steady-state refresh: %v", err)
	} else if n2 != 0 {
		t.Errorf("re-refreshed after late chunk settled: %d, want 0", n2)
	}
}
