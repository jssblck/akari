package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

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
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
			{Ordinal: 2, Role: "user", Content: "one more thing"},
		},
		// A rebuild while still live grades in-line only once idle long enough, so backdate
		// ended_at afterward: the rebuild leaves signals_stale set (a live rebuild never
		// clears it), matching production where a session settles quietly between rebuilds.
	})
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 2, ended_at = now() - make_interval(mins => $2) WHERE id = $1",
		sid, endedMinsAgo); err != nil {
		t.Fatalf("set session facts for %s: %v", src, err)
	}
	return sid
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

// TestRefreshSettledSignalsGradesTerminalImmediately pins the server-side half of
// `akari sync --finalize`: a session the client declared terminal is due and grades on the
// next settle pass even though it ended only minutes ago, nowhere near the 30-minute
// abandoned-idle window. The same recent session without the flag stays ungraded (the fresh
// case in TestRefreshSettledSignalsMaterializesSettledOnly), so this isolates the terminal
// shortcut in both dueSettledBatch (it is selected) and gatherSignalFacts (idleLongEnough is
// forced, so Classify renders a verdict). The user-last fixture reads abandoned once idle
// long enough; terminal makes that verdict land now.
func TestRefreshSettledSignalsGradesTerminalImmediately(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-terminal", 5) // ended 5 min ago: not settled
	terminal := seedSettledSession(t, st, ctx, uid, pid, "sess-terminal-flagged", 5)
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET terminal = true WHERE id = $1", terminal); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh settled: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d, want 1 (only the terminal session, despite both ending recently)", n)
	}
	if got := signalsRowCount(t, st, ctx, sid); got != 0 {
		t.Errorf("non-terminal recent session graded (row count %d), want 0 until it settles", got)
	}
	sig, err := st.SessionSignalsByID(ctx, terminal)
	if err != nil {
		t.Fatalf("read terminal signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeAbandoned) {
		t.Errorf("terminal outcome = %s, want abandoned (graded now, not held pre-settle)", sig.Outcome)
	}
	// The grade cleared signals_stale, so a terminal session drops out of the due set the
	// same way a settled one does: a second pass finds nothing due.
	if n2, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	} else if n2 != 0 {
		t.Errorf("second pass refreshed %d, want 0 (the terminal grade is stable)", n2)
	}
}

// TestRefreshSettledSignalsGradesTerminalWithNullEndedAt pins the scope match between the
// terminal derivation and the terminal drain. gatherSignalFacts treats any terminal session as
// idle-long-enough, so a terminal transcript that parsed messages but carries no timestamp (a
// NULL ended_at) is gradeable. The settled-by-idle drain orders by ended_at and so can never
// select a NULL-ended row, which would strand such a session ungraded whenever the explicit
// finalize call was missed; the terminal drain (keyed on id) must grade it. Without the separate
// drain this session would never materialize a signal from the settle pass.
func TestRefreshSettledSignalsGradesTerminalWithNullEndedAt(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-terminal-null")
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
		},
	})
	// Terminal, one human turn, and no ended_at at all: the settled drain cannot see it.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 1, terminal = true, ended_at = NULL WHERE id = $1", sid); err != nil {
		t.Fatalf("mark terminal with NULL ended_at: %v", err)
	}

	n, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		t.Fatalf("refresh settled: %v", err)
	}
	if n != 1 {
		t.Errorf("refreshed %d, want 1 (the terminal drain must grade a NULL-ended terminal session)", n)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeCompleted) {
		t.Errorf("outcome = %s, want completed (assistant had the last substantive word)", sig.Outcome)
	}
	// Stable: the grade cleared signals_stale, so a second pass finds nothing due.
	if n2, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	} else if n2 != 0 {
		t.Errorf("second pass refreshed %d, want 0 (the terminal grade is stable)", n2)
	}
}

// TestRefreshSettledSignalsCorrectsPreSettleRow pins the correction the whole settle design
// exists to prevent: a grade taken before the session settled carries a not-yet-final outcome,
// and the settle pass must revisit it once the outcome stabilizes. A rebuild of a still-live
// session leaves signals_stale set (see rebuildTx), so the settle pass re-grades it once it
// settles. A second pass then finds nothing due, so the corrected outcome holds and does not
// oscillate.
func TestRefreshSettledSignalsCorrectsPreSettleRow(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-presettle")

	// Rebuild it while still live (delta.Ended zero, so ended_at stays NULL): the outcome is
	// not yet abandoned (not idle long enough), and because the session is not settled the
	// rebuild must leave signals_stale set, so the settle pass will revisit it.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
			{Ordinal: 2, Role: "user", Content: "one more thing"},
		},
	})
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET user_message_count = 2 WHERE id = $1", sid); err != nil {
		t.Fatalf("set user message count: %v", err)
	}
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
// and it would reflect only the partial upload forever. A rebuild always sets signals_stale on
// entry (see rebuildTx), so the late chunk's rebuild re-marks the session stale and the pass
// recomputes it, reflecting the added content. The test drives both chunks through rebuildWith,
// combining them into one delta the second time (a rebuild replaces the whole projection, so the
// later rebuild must carry every row the session should end up with).
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

	// A later chunk of the same historical transcript appends another human turn. The rebuild
	// replaces the whole projection, so the combined delta carries every original row plus the
	// new turns; ended_at stays far in the past (still settled) but the rebuild re-marks
	// signals_stale on entry, so the settle pass must recompute it.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
			{Ordinal: 2, Role: "user", Content: "one more thing"},
			{Ordinal: 3, Role: "assistant", Content: "sure"},
			{Ordinal: 4, Role: "user", Content: "and one more real request please"},
		},
	})
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 3, ended_at = now() - make_interval(mins => 6000) WHERE id = $1", sid); err != nil {
		t.Fatalf("set session facts for late chunk: %v", err)
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

// TestRefreshSettledSignalsSkipsProjectionAheadOfEpoch pins the rolling-deploy guard on
// the settle path: a projection stamped by a NEWER binary must not be graded by an older
// one. The old binary's scoring code does not match the newer projection, and grading
// would clear signals_stale, leaving no marker for the newer binary to redo the grade
// (the session is not due to it either, since its projection is current). The skip
// leaves signals_stale set, and the pass at the newer epoch grades it normally.
func TestRefreshSettledSignalsSkipsProjectionAheadOfEpoch(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-ahead", 120)
	// Stand in for a newer binary's rebuild: the projection is stamped one epoch
	// ahead of the epoch this store instance runs at.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE session_raw SET parser_epoch = $2 WHERE session_id = $1", sid, testEpoch+1); err != nil {
		t.Fatalf("stamp projection ahead: %v", err)
	}
	st.SetParserEpoch(testEpoch)

	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh at the older epoch: %v", err)
	}
	if got := signalsRowCount(t, st, ctx, sid); got != 0 {
		t.Errorf("older-epoch settle pass graded a newer projection (row count %d), want 0", got)
	}
	var stale bool
	if err := st.Pool.QueryRow(ctx, "SELECT signals_stale FROM sessions WHERE id = $1", sid).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("the skip must leave signals_stale set so the newer binary's tick grades it")
	}

	// Once this instance runs the epoch that built the projection (the deploy
	// finished), the same pass grades it.
	st.SetParserEpoch(testEpoch + 1)
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh at the newer epoch: %v", err)
	}
	if got := signalsRowCount(t, st, ctx, sid); got != 1 {
		t.Errorf("newer-epoch settle pass signals row count = %d, want 1", got)
	}
}

// TestRefreshSettledSignalsSkipsPendingRebuild pins the finalize race guard: raw
// bytes past the last rebuild mean a rebuild is due, so a signal refresh (a
// finalize request racing the parse worker, or a settle tick landing in the gap)
// must skip rather than grade a projection that does not cover the bytes and
// clear signals_stale as if it did. The pending rebuild then grades the settled
// session itself, in the transaction that parses the bytes.
func TestRefreshSettledSignalsSkipsPendingRebuild(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSettledSession(t, st, ctx, uid, pid, "sess-pending", 120)
	if _, err := st.AppendChunk(ctx, sid, 0, []byte("late chunk\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	st.SetParserEpoch(testEpoch)

	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh with dirty bytes: %v", err)
	}
	if got := signalsRowCount(t, st, ctx, sid); got != 0 {
		t.Errorf("refresh graded a projection with unparsed bytes pending (row count %d), want 0", got)
	}
	var stale bool
	if err := st.Pool.QueryRow(ctx, "SELECT signals_stale FROM sessions WHERE id = $1", sid).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("the skip must leave signals_stale set for the rebuild to settle")
	}

	// The due rebuild covers the bytes and, since the session is settled (the
	// delta's Ended keeps the rederived ended_at past the idle window), grades
	// it in the same transaction: no separate refresh needed.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "late chunk"}},
		Started:  time.Now().Add(-3 * time.Hour),
		Ended:    time.Now().Add(-2 * time.Hour),
	})
	if got := signalsRowCount(t, st, ctx, sid); got != 1 {
		t.Errorf("rebuild of the settled session did not grade it (row count %d), want 1", got)
	}
}

// TestRefreshSettledSignalsHonorsFailurePins pins the two failure-marker cases of
// the grading guard. A deterministic failure PINNED AT THE RUNNING EPOCH is
// gradeable: the drain can never advance the session, so the settle pass grades
// the surviving projection under the current scoring (the failure model's
// contract; without this, the byte-dirty skip would strand every failed session
// ungraded forever). A failure recorded AHEAD of the running epoch is the
// opposite: it is a newer binary's attempt, and the older binary must leave the
// grade alone the same way it leaves a newer successful stamp alone.
func TestRefreshSettledSignalsHonorsFailurePins(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	pinnedCurrent := seedSettledSession(t, st, ctx, uid, pid, "sess-pin-current", 120)
	pinnedAhead := seedSettledSession(t, st, ctx, uid, pid, "sess-pin-ahead", 120)

	// pinnedCurrent: new bytes arrive and the running parser rejects them, so the
	// failure covers the current bytes at the running epoch.
	if _, err := st.AppendChunk(ctx, pinnedCurrent, 0, []byte("bad bytes\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	rerr := errors.New("malformed transcript")
	if err := st.RebuildSession(ctx, pinnedCurrent, testEpoch, failingReducer{rerr}); !errors.Is(err, rerr) {
		t.Fatalf("failing rebuild returned %v, want the reducer's error", err)
	}
	// pinnedAhead: a newer binary's failed attempt, recorded without advancing
	// the success bookkeeping (byte_len is 0: no raw was uploaded, so the pin
	// covers the current bytes trivially).
	if _, err := st.Pool.Exec(ctx,
		`UPDATE session_raw SET parse_error = 'newer parser rejected', parse_error_epoch = $2 WHERE session_id = $1`,
		pinnedAhead, testEpoch+1); err != nil {
		t.Fatalf("pin ahead: %v", err)
	}
	st.SetParserEpoch(testEpoch)

	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := signalsRowCount(t, st, ctx, pinnedCurrent); got != 1 {
		t.Errorf("failure pinned at the running epoch was not graded (row count %d), want 1: failed sessions must not be stranded ungraded", got)
	}
	if got := signalsRowCount(t, st, ctx, pinnedAhead); got != 0 {
		t.Errorf("failure pinned ahead of the running epoch was graded (row count %d), want 0", got)
	}

	// The instance running the epoch that recorded the ahead pin grades the
	// surviving projection normally.
	st.SetParserEpoch(testEpoch + 1)
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh at the newer epoch: %v", err)
	}
	if got := signalsRowCount(t, st, ctx, pinnedAhead); got != 1 {
		t.Errorf("ahead pin at its own epoch was not graded (row count %d), want 1", got)
	}
}
