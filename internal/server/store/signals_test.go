package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// signalsEnv stands up a user and project once so each signals test can seed its own
// session against them without repeating the registration boilerplate.
func signalsEnv(t *testing.T) (*store.Store, context.Context, int64, int64) {
	t.Helper()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	return st, ctx, u.ID, pid
}

// setUserMessageCount stamps the user-turn rollup that ApplyProjectionDelta (which does
// not touch session aggregates) leaves at zero, so the outcome classifier sees a human
// turn. The live and reparse paths maintain this through applyAggregates; here the test
// sets it directly to isolate the signal computation.
func setUserMessageCount(t *testing.T, st *store.Store, ctx context.Context, sid int64, n int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET user_message_count = $2 WHERE id = $1", sid, n); err != nil {
		t.Fatalf("set user_message_count: %v", err)
	}
}

// markSignalsFresh clears signals_stale on a session, the flag the read gate keys on. A test
// that seeds a session_signals row directly is standing in for a settled, graded session, whose
// flag the settle pass would have cleared, so the aggregates and the header must read the seeded
// row rather than treat it as behind the projection. The signal-insert helpers call this so a
// seeded grade is visible; a stale-version seed stays excluded through the version filter, not
// the flag.
func markSignalsFresh(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = false WHERE id = $1`, sid); err != nil {
		t.Fatalf("clear signals_stale for session %d: %v", sid, err)
	}
}

// settleSession backdates a session's last activity past the abandoned idle window so the grade
// RefreshSessionSignals writes clears signals_stale and the per-session read returns it. In
// production signals only materialize for settled sessions (the settle pass), and a grade taken
// while a session is still live is held back as pre-settle (see SessionSignalsByID), so a reducer
// test that reads its grade back must settle first. Backdating to an hour keeps an idle-sensitive
// outcome (abandoned) settled while leaving a resolved session completed and a failing tail
// errored, so it changes no asserted outcome.
func settleSession(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET ended_at = now() - interval '1 hour' WHERE id = $1", sid); err != nil {
		t.Fatalf("settle session: %v", err)
	}
}

// TestSignalsToolHealthCounts exercises the whole SQL fact computation over a session
// that fails, retries, churns, and runs a failure streak but recovers by the end, so
// the counts and the resulting score can be pinned exactly. The deliberate ordering
// (errors in the middle, an ok call last) keeps the outcome "completed", isolating the
// tool-health arithmetic from the errored-tail rule.
func TestSignalsToolHealthCounts(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-tool-health")

	editA := "edit-file-a"
	editB := "edit-file-b"
	bash := "bash-input"
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "do the work"},
			{Ordinal: 1, Role: "assistant", Content: "done", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: editA, CallUID: "a"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: editA, CallUID: "b"}, // immediate retry of call 0
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Bash", Category: "bash", InputBody: bash, CallUID: "c"},
			{MessageOrdinal: 1, CallIndex: 3, ToolName: "Bash", Category: "bash", InputBody: bash, CallUID: "d"}, // retry of call 2
			{MessageOrdinal: 1, CallIndex: 4, ToolName: "Bash", Category: "bash", InputBody: bash, CallUID: "e"}, // retry of call 3 (3-error streak)
			{MessageOrdinal: 1, CallIndex: 5, ToolName: "Edit", Category: "edit", FilePath: "b.go", InputBody: editB, CallUID: "f"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "a", Status: "ok"},
			{CallUID: "b", Status: "ok"},
			{CallUID: "c", Status: "error"},
			{CallUID: "d", Status: "error"},
			{CallUID: "e", Status: "error"},
			{CallUID: "f", Status: "ok"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Version != quality.Version {
		t.Errorf("signals_version = %d, want %d", sig.Version, quality.Version)
	}
	if sig.ToolCalls != 6 || sig.ToolFailures != 3 || sig.ToolRetries != 3 || sig.EditChurn != 1 || sig.LongestFailureStreak != 3 {
		t.Errorf("counts = {calls %d, fail %d, retry %d, churn %d, streak %d}, want {6, 3, 3, 1, 3}",
			sig.ToolCalls, sig.ToolFailures, sig.ToolRetries, sig.EditChurn, sig.LongestFailureStreak)
	}
	if sig.Outcome != string(quality.OutcomeCompleted) || sig.OutcomeConfidence != string(quality.ConfHigh) {
		t.Errorf("outcome = (%s, %s), want (completed, high)", sig.Outcome, sig.OutcomeConfidence)
	}
	// 100 - failures(3*3=9) - retries(3*5=15) - churn(1*4=4) - streak(10) = 62 -> C
	if !sig.Scored() || *sig.Score != 62 || *sig.Grade != "C" {
		t.Errorf("score/grade = (%v, %v, scored=%v), want (62, C, true)", sig.Score, sig.Grade, sig.Scored())
	}
}

// TestSignalsDedupesReplayedCalls confirms the signal computation counts the deduped
// tool calls, not the raw rows: a resumed or compacted Claude transcript replays a
// call's id verbatim across several rows, and counting each would inflate the calls and
// failures. Three replayed copies of one call collapse to one; a genuinely distinct
// call still counts.
func TestSignalsDedupesReplayedCalls(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-dedup")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "first", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", Content: "replay one", HasToolUse: true},
			{Ordinal: 3, Role: "assistant", Content: "replay two", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			// One call replayed across three turns: same id, tool, input, and (after
			// back-patch) result, so the dedup collapses them to one.
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "dup"},
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "dup"},
			{MessageOrdinal: 3, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "dup"},
			// A genuinely distinct failing call.
			{MessageOrdinal: 3, CallIndex: 1, ToolName: "Bash", Category: "bash", CallUID: "x"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "dup", Status: "ok"},
			{CallUID: "x", Status: "error"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	// Four physical rows, but the three replays of "dup" count once: 2 calls, 1 failure.
	if sig.ToolCalls != 2 || sig.ToolFailures != 1 {
		t.Errorf("deduped counts = {calls %d, fail %d}, want {2, 1} (replays must not inflate)", sig.ToolCalls, sig.ToolFailures)
	}
}

// TestSignalsDistinctNullCallsNotCollapsed guards the dedup namespace: a NULL call_uid
// must never group, even with another NULL-call row that is otherwise identical, and a
// real call_uid that resembles the synthetic per-row key ("1:0") must not collide with a
// NULL-call row's discriminator. All three calls here are distinct and must count.
func TestSignalsDistinctNullCallsNotCollapsed(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-null-calls")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "working", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: ""},    // no id
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", Category: "read", CallUID: ""},    // no id, otherwise identical
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Read", Category: "read", CallUID: "1:0"}, // real id resembling a synthetic key
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.ToolCalls != 3 {
		t.Errorf("tool_calls = %d, want 3 (no-id calls must never collapse, and a real id must not collide)", sig.ToolCalls)
	}
}

// TestSignalsEditChurnIgnoresUnknownPath pins the churn rule: an edit whose file path
// did not parse is excluded rather than counted as its own churn, so two unknown-path
// edits add nothing while a genuine repeat edit to one known file adds one.
func TestSignalsEditChurnIgnoresUnknownPath(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-churn-nullpath")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit things"},
			{Ordinal: 1, Role: "assistant", Content: "edited", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "a.go", CallUID: "e1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "a.go", CallUID: "e2"}, // repeat edit to a.go -> churn 1
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Edit", Category: "edit", FilePath: "", CallUID: "e3"},     // unknown path, excluded
			{MessageOrdinal: 1, CallIndex: 3, ToolName: "Edit", Category: "edit", FilePath: "", CallUID: "e4"},     // unknown path, excluded
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "e1", Status: "ok"}, {CallUID: "e2", Status: "ok"},
			{CallUID: "e3", Status: "ok"}, {CallUID: "e4", Status: "ok"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.ToolCalls != 4 || sig.EditChurn != 1 {
		t.Errorf("counts = {calls %d, churn %d}, want {4, 1} (unknown-path edits add no churn)", sig.ToolCalls, sig.EditChurn)
	}
}

// TestSignalsErroredOutcome confirms a session that ends on a run of failing tool calls
// classifies as errored regardless of who spoke last, and takes the errored penalty.
func TestSignalsErroredOutcome(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-errored")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "run it"},
			{Ordinal: 1, Role: "assistant", Content: "trying", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Bash", Category: "bash", CallUID: "e1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Bash", Category: "bash", CallUID: "e2"},
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Bash", Category: "bash", CallUID: "e3"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "e1", Status: "error"},
			{CallUID: "e2", Status: "error"},
			{CallUID: "e3", Status: "error"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeErrored) || sig.OutcomeConfidence != string(quality.ConfHigh) {
		t.Errorf("outcome = (%s, %s), want (errored, high)", sig.Outcome, sig.OutcomeConfidence)
	}
	// 100 - errored(30) - failures(3*3=9) - streak(10) = 51 -> D
	if !sig.Scored() || *sig.Score != 51 || *sig.Grade != "D" {
		t.Errorf("score/grade = (%v, %v), want (51, D)", sig.Score, sig.Grade)
	}
}

// TestSignalsAbandonedOutcome confirms a session whose last substantive turn is the
// user's, gone quiet past the idle threshold, reads as abandoned and takes only the
// abandoned penalty (a pure-conversation session has no tool signal to grade).
func TestSignalsAbandonedOutcome(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-abandoned")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "first ask"},
			{Ordinal: 1, Role: "assistant", Content: "here you go"},
			{Ordinal: 2, Role: "user", Content: "one more thing"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	// Two human turns and a last activity well past the abandoned idle window.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET user_message_count = 2, ended_at = now() - interval '1 hour' WHERE id = $1", sid); err != nil {
		t.Fatalf("set session facts: %v", err)
	}
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeAbandoned) || sig.OutcomeConfidence != string(quality.ConfMedium) {
		t.Errorf("outcome = (%s, %s), want (abandoned, medium)", sig.Outcome, sig.OutcomeConfidence)
	}
	if !sig.Scored() || *sig.Score != 85 || *sig.Grade != "B" {
		t.Errorf("score/grade = (%v, %v), want (85, B)", sig.Score, sig.Grade)
	}
}

// TestSignalsUnknownIsUnscored confirms a session that stays unknown even once settled, an
// automation run (no human turn) whose only assistant turn carried no substantive content
// (tool plumbing, not a delivered answer), and with no tool signal, is left unscored: the
// read returns an unknown outcome with nil score and grade, the same restraint the UI shows
// rather than inventing a verdict. Under the v2 classifier a settled automation run with a
// substantive assistant last word reads as completed, so this fixture deliberately gives no
// substantive assistant turn (content_length 0), the case v2 still leaves unknown.
func TestSignalsUnknownIsUnscored(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-unknown")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			// An empty assistant turn: it carried a tool call but no prose, so content_length
			// is 0 and LastAssistantOrd stays -1, leaving nothing substantive to read.
			{Ordinal: 0, Role: "assistant", Content: "", HasToolUse: true},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeUnknown) {
		t.Errorf("outcome = %s, want unknown", sig.Outcome)
	}
	if sig.Scored() || sig.Score != nil || sig.Grade != nil {
		t.Errorf("unknown no-signal session should be unscored, got score=%v grade=%v", sig.Score, sig.Grade)
	}
}

// TestSignalsClearedOnReset confirms a raw reset drops the signals row with the rest of
// the projection, so a session about to be re-parsed from zero does not keep a stale
// verdict. The settle pass rebuilds it once the re-parsed session settles.
func TestSignalsClearedOnReset(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-reset")

	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hi"},
			{Ordinal: 1, Role: "assistant", Content: "hello"},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}
	if rows := signalsRowCount(t, st, ctx, sid); rows != 1 {
		t.Fatalf("expected a signals row before reset, got %d", rows)
	}

	if err := st.ResetRaw(ctx, sid); err != nil {
		t.Fatalf("reset raw: %v", err)
	}
	if rows := signalsRowCount(t, st, ctx, sid); rows != 0 {
		t.Errorf("signals row survived a reset, count = %d, want 0", rows)
	}
	// The read self-heals to an unknown, unscored result rather than erroring.
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals after reset: %v", err)
	}
	if sig.Scored() || sig.Outcome != string(quality.OutcomeUnknown) {
		t.Errorf("post-reset read = (%s, scored=%v), want (unknown, false)", sig.Outcome, sig.Scored())
	}
}

// TestSignalsBuiltByReparse confirms the reparse path computes signals end to end: the
// reduce-driven replay maintains the aggregates and, on its final region, refreshes the
// signals, so a reparse (the versioned backfill) leaves every session with a current
// row. This is the path an Epoch bump runs across the whole corpus.
func TestSignalsBuiltByReparse(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: "sess-reparse-signals", ProjectID: pid,
		GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID
	if _, err := st.AppendChunk(ctx, sid, 0, []byte("one transcript line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}

	// A reduce that emits a clean completed session: a user ask, an assistant reply, and
	// two resolved Read calls. The reparse drives this and then computes the signals.
	emit := func(_, _ []byte, _ int64) ([]byte, store.ProjectionDelta, error) {
		return []byte("{}"), store.ProjectionDelta{
			Messages: []store.MessageDelta{
				{Ordinal: 0, Role: "user", Content: "please read"},
				{Ordinal: 1, Role: "assistant", Content: "read it", HasToolUse: true},
			},
			ToolCalls: []store.ProjToolCall{
				{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "r1"},
				{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", Category: "read", CallUID: "r2"},
			},
			ToolResults: []store.ToolResultDelta{
				{CallUID: "r1", Status: "ok"},
				{CallUID: "r2", Status: "ok"},
			},
			// Backdate the region past the abandoned idle window so the reparse's own grade
			// lands settled (signals_stale cleared) and the read below returns it, the same
			// way a real historical transcript carries past timestamps.
			Ended: time.Now().Add(-time.Hour),
		}, nil
	}
	if err := st.ReparseSession(ctx, sid, 3, emit); err != nil {
		t.Fatalf("reparse: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeCompleted) {
		t.Errorf("reparse outcome = %s, want completed", sig.Outcome)
	}
	if !sig.Scored() || *sig.Score != 100 || *sig.Grade != "A" || sig.ToolCalls != 2 {
		t.Errorf("reparse signals = {score %v, grade %v, calls %d}, want {100, A, 2}", sig.Score, sig.Grade, sig.ToolCalls)
	}
}

// TestSignalsPromptHygiene drives the whole hygiene path: the refresh reads the session's
// human prompts in order, classifies them, and stores the counts. The seeded prompts are
// chosen to trip each signal exactly once or twice so the stored row can be pinned: a
// terse opener (short and unstructured start), a repeated real request (a duplicate, both
// copies naming no code), and a clean anchored request that trips nothing.
func TestSignalsPromptHygiene(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-hygiene")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hey"}, // terse opener
			{Ordinal: 1, Role: "assistant", Content: "on it"},
			{Ordinal: 2, Role: "user", Content: "add pagination to the sessions list"}, // no code anchor
			{Ordinal: 3, Role: "assistant", Content: "done"},
			{Ordinal: 4, Role: "user", Content: "add pagination to the sessions list"}, // verbatim repeat
			{Ordinal: 5, Role: "assistant", Content: "done again"},
			{Ordinal: 6, Role: "user", Content: "now refactor the loop in internal/server/store/signals.go"}, // anchored, clean
			{Ordinal: 7, Role: "assistant", Content: "refactored"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 4)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	// Four non-empty human prompts, so the classifier base is 4.
	if sig.PromptCount != 4 {
		t.Errorf("prompt_count = %d, want 4 (the non-empty human prompts)", sig.PromptCount)
	}
	// "hey" is the only terse prompt; the second "add pagination" line is the duplicate;
	// both "add pagination" lines name no code (the refactor line names a file, so it is
	// clean); the opener is terse, so the start is unstructured.
	if sig.ShortPromptCount != 1 || sig.DuplicatePromptCount != 1 ||
		sig.NoCodeContextCount != 2 || !sig.UnstructuredStart {
		t.Errorf("hygiene = {short %d, dup %d, nocode %d, unstructured %v}, want {1, 1, 2, true}",
			sig.ShortPromptCount, sig.DuplicatePromptCount, sig.NoCodeContextCount, sig.UnstructuredStart)
	}
	if !sig.HasHygieneSignal() {
		t.Error("HasHygieneSignal should be true when any hygiene count fired")
	}
}

// TestSignalsSkipUnbackfilledPromptFacts pins the pre-reparse guard. Migration 0022 adds the
// per-message hygiene columns but cannot backfill them, so a session ingested before it reads NULL
// facts until the Epoch reparse re-inserts its messages through the classifier. If the settle pass
// reaches such a settled session first, it must leave it ungraded and stale rather than record a
// hollow all-zero hygiene row and clear the flag, which would read as measured-clean and mask the
// real signal. Once the facts are materialized (as the reparse fills them), the same refresh grades
// the session normally.
func TestSignalsSkipUnbackfilledPromptFacts(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-prereparse")

	const opener = "please refactor the retry loop in internal/server/store/signals.go"
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: opener},
			{Ordinal: 1, Role: "assistant", Content: "done"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)
	// Simulate a message written before migration 0022 added the columns: NULL every prompt fact the
	// insert would have computed, and mark the session due as the settle pass would find it.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages
		    SET prompt_short = NULL, prompt_no_code = NULL, prompt_bare_greeting = NULL, prompt_digest = NULL
		  WHERE session_id = $1 AND role = 'user'`, sid); err != nil {
		t.Fatalf("null prompt facts: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("mark stale: %v", err)
	}

	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (pre-backfill): %v", err)
	}

	// The refresh must have written no row (ungradeable until a reparse fills the facts) and CLEARED
	// signals_stale so the settle pass stops re-scanning this session's history every wake. The reparse
	// that fills the facts re-marks it due through applyAggregates and grades it then, so dropping it
	// from the due set now loses no grade, it only avoids the O(ticks * history) polling.
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM session_signals WHERE session_id = $1`, sid).Scan(&rows); err != nil {
		t.Fatalf("count signals rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("a pre-reparse session must stay ungraded; got %d session_signals row(s)", rows)
	}
	var stale bool
	if err := st.Pool.QueryRow(ctx, `SELECT signals_stale FROM sessions WHERE id = $1`, sid).Scan(&stale); err != nil {
		t.Fatalf("read signals_stale: %v", err)
	}
	if stale {
		t.Error("an ungradeable pre-reparse session must be dropped from the settle-due set (signals_stale=false), not re-scanned every wake")
	}

	// Materialize the facts as the reparse's message re-insert would, then re-grade: the session is
	// now ready and grades normally. The insert stamps quality.PromptFactsVersion alongside the
	// verdicts, so the backfill must set it too; a row left at the default NULL version reads as
	// unmeasured (see the stale-version guard in TestSignalsSkipStalePromptFactsVersion).
	facts := quality.ClassifyPrompt(opener)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages
		    SET prompt_short = $2, prompt_no_code = $3, prompt_bare_greeting = $4, prompt_digest = $5,
		        prompt_facts_version = $6
		  WHERE session_id = $1 AND role = 'user'`,
		sid, facts.Short, facts.NoCodeContext, facts.BareGreeting, facts.Digest, quality.PromptFactsVersion); err != nil {
		t.Fatalf("backfill prompt facts: %v", err)
	}
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (post-backfill): %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.Scored() {
		t.Errorf("a backfilled settled session should grade; got unscored %+v", sig)
	}
	// The opener is anchored and substantive, so no hygiene flag fires and the base is the one prompt.
	if sig.PromptCount != 1 || sig.HasHygieneSignal() {
		t.Errorf("hygiene = {prompts %d, hasSignal %v}, want {1, false}", sig.PromptCount, sig.HasHygieneSignal())
	}
}

// TestSignalsSkipStalePromptFactsVersion pins the version half of the pre-reparse guard. Filled facts
// are not enough: a change to ClassifyPrompt's rules bumps quality.PromptFactsVersion, and a message
// still carrying facts at the superseded version was classified by the old rules. Mixing those into a
// hygiene count would blend classifier versions, so gatherPromptHygiene treats an old-version row like
// an unfilled one and leaves the session ungraded until the paired Epoch reparse re-derives the facts
// at the current version. This is the version companion to the NULL-facts guard above.
func TestSignalsSkipStalePromptFactsVersion(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-staleversion")

	const opener = "please refactor the retry loop in internal/server/store/signals.go"
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: opener},
			{Ordinal: 1, Role: "assistant", Content: "done"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)

	// The insert filled facts at the current version. Rewind just the version stamp to the prior one,
	// standing in for a message classified by superseded rules that the reparse has not yet reached,
	// and mark the session due as the settle pass would find it.
	facts := quality.ClassifyPrompt(opener)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages
		    SET prompt_short = $2, prompt_no_code = $3, prompt_bare_greeting = $4, prompt_digest = $5,
		        prompt_facts_version = $6
		  WHERE session_id = $1 AND role = 'user'`,
		sid, facts.Short, facts.NoCodeContext, facts.BareGreeting, facts.Digest, quality.PromptFactsVersion-1); err != nil {
		t.Fatalf("rewind prompt facts version: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("mark stale: %v", err)
	}

	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (stale version): %v", err)
	}

	// A stale-version session must stay ungraded and, like a NULL-facts one, be dropped from the due set
	// so the settle pass stops re-scanning it; the reparse that advances its facts version re-marks it.
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM session_signals WHERE session_id = $1`, sid).Scan(&rows); err != nil {
		t.Fatalf("count signals rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("a stale-facts-version session must stay ungraded; got %d session_signals row(s)", rows)
	}
	var stale bool
	if err := st.Pool.QueryRow(ctx, `SELECT signals_stale FROM sessions WHERE id = $1`, sid).Scan(&stale); err != nil {
		t.Fatalf("read signals_stale: %v", err)
	}
	if stale {
		t.Error("an ungradeable stale-facts-version session must be dropped from the settle-due set (signals_stale=false), not re-scanned every wake")
	}

	// Advance the stamp to the current version, as the reparse's re-insert would, then re-grade: the
	// facts are now current and the session grades normally.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages SET prompt_facts_version = $2 WHERE session_id = $1 AND role = 'user'`,
		sid, quality.PromptFactsVersion); err != nil {
		t.Fatalf("advance prompt facts version: %v", err)
	}
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (current version): %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.Scored() {
		t.Errorf("a current-version settled session should grade; got unscored %+v", sig)
	}
}

// TestSignalsGuardKeepsStaleGradeHidden pins the projection-consistency half of the pre-reparse
// guard. The guard drops an ungradeable session out of the settle-due set by clearing signals_stale,
// but only when no current-version session_signals row could be re-exposed by the clear. A session
// can grade cleanly (leaving a current-version row), then take new activity that marks it stale
// through applyAggregates, and then have its prompt facts superseded by a quality.PromptFactsVersion
// bump the reparse has not yet reached. That stored row now reflects the pre-activity projection, so
// clearing signals_stale would let it pass the NOT signals_stale AND signals_version = quality.Version
// read gate again and serve a stale grade as current. This walks a session through exactly that
// sequence and asserts the guard leaves signals_stale set, then that the session recovers once the
// facts are current again. It is the companion to the two guards above, which cover the never-graded
// case where clearing IS safe.
func TestSignalsGuardKeepsStaleGradeHidden(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-stalegrade-hidden")

	const opener = "please refactor the retry loop in internal/server/store/signals.go"
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: opener},
			{Ordinal: 1, Role: "assistant", Content: "done"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	settleSession(t, st, ctx, sid)

	// Grade it cleanly first: facts are current at insert, so this writes a current-version row and
	// clears signals_stale. This is the session that graded once, before its classifier is superseded.
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (initial grade): %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.Scored() {
		t.Fatalf("setup: the session must grade first; got unscored %+v", sig)
	}

	// New activity since that grade marked the session stale (applyAggregates does this on the live and
	// reparse paths); set it directly here. Then supersede the classifier by rewinding the messages'
	// prompt_facts_version, standing in for a PromptFactsVersion bump the reparse has not reached, so
	// the next refresh reaches the pre-reparse guard with a current-version row already present.
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	facts := quality.ClassifyPrompt(opener)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages
		    SET prompt_short = $2, prompt_no_code = $3, prompt_bare_greeting = $4, prompt_digest = $5,
		        prompt_facts_version = $6
		  WHERE session_id = $1 AND role = 'user'`,
		sid, facts.Short, facts.NoCodeContext, facts.BareGreeting, facts.Digest, quality.PromptFactsVersion-1); err != nil {
		t.Fatalf("rewind prompt facts version: %v", err)
	}

	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (stale facts over graded row): %v", err)
	}

	// The guard must NOT have cleared signals_stale: a current-version row exists, so clearing it would
	// re-expose a grade that predates the activity which marked the session stale. The flag stays set,
	// which keeps the row out of every fleet read's NOT signals_stale gate, and the session stays due.
	var stale bool
	if err := st.Pool.QueryRow(ctx, `SELECT signals_stale FROM sessions WHERE id = $1`, sid).Scan(&stale); err != nil {
		t.Fatalf("read signals_stale: %v", err)
	}
	if !stale {
		t.Error("a graded session whose facts went stale must stay signals_stale=true so its pre-activity grade is not re-exposed")
	}
	// The prior grade row is left in place (the guard returns before the upsert), not deleted: it is
	// hidden by the flag, then re-derived by the reparse, never dropped.
	var rows int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM session_signals WHERE session_id = $1`, sid).Scan(&rows); err != nil {
		t.Fatalf("count signals rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the prior grade row must be left in place, not deleted; got %d row(s)", rows)
	}

	// When the reparse advances the facts to the current version, the next refresh grades normally and
	// clears the flag: the session recovers rather than staying hidden forever.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE messages SET prompt_facts_version = $2 WHERE session_id = $1 AND role = 'user'`,
		sid, quality.PromptFactsVersion); err != nil {
		t.Fatalf("advance prompt facts version: %v", err)
	}
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals (recovered facts): %v", err)
	}
	var staleAfter bool
	if err := st.Pool.QueryRow(ctx, `SELECT signals_stale FROM sessions WHERE id = $1`, sid).Scan(&staleAfter); err != nil {
		t.Fatalf("read signals_stale after recovery: %v", err)
	}
	if staleAfter {
		t.Error("once facts are current again the session must re-grade and drop out of the due set")
	}
}

// setPromptFactsVersion overwrites a graded row's stored classifier version, standing in for a
// session_signals aggregate that a ClassifyPrompt change (a quality.PromptFactsVersion bump) left at
// a superseded classifier version before the paired epoch reparse re-derived it.
func setPromptFactsVersion(t *testing.T, st *store.Store, ctx context.Context, sid int64, v int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`UPDATE session_signals SET prompt_facts_version = $2 WHERE session_id = $1`, sid, v); err != nil {
		t.Fatalf("set prompt_facts_version: %v", err)
	}
}

// TestPromptFactsVersionHidesStaleHygiene pins the read-side invariant for the classifier version.
// session_signals.prompt_facts_version records the quality.PromptFactsVersion the row's hygiene counts
// were derived under; it is separate from signals_version, so a scoring-version-current row can still
// hold hygiene from a superseded classifier until the reparse re-derives it. Both hygiene reads gate on
// it: SessionSignalsByID marks hygiene unmeasured (so the session page hides it) and the fleet aggregate
// excludes the row, while the non-hygiene signals (outcome, score) stay visible because they do not
// depend on the classifier. This walks a graded session through a classifier bump and back, asserting
// the hygiene is hidden until re-derived and the grade is never dropped along the way.
func TestPromptFactsVersionHidesStaleHygiene(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-factsversion-hide")

	// A terse opener (short and unstructured) and a clean anchored follow-up, so hygiene actually
	// fires and the row has a signal to hide.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hey"},
			{Ordinal: 1, Role: "assistant", Content: "on it"},
			{Ordinal: 2, Role: "user", Content: "please refactor the retry loop in internal/server/store/signals.go"},
			{Ordinal: 3, Role: "assistant", Content: "done"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 2)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("grade: %v", err)
	}

	// Graded at the current classifier version: hygiene is measured and the fired signal shows, both
	// per-session and in the fleet aggregate.
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.HygieneMeasured || !sig.HasHygieneSignal() || sig.ShortPromptCount != 1 || !sig.UnstructuredStart {
		t.Fatalf("current-version hygiene = {measured %v, hasSignal %v, short %d, unstructured %v}, want {true, true, 1, true}",
			sig.HygieneMeasured, sig.HasHygieneSignal(), sig.ShortPromptCount, sig.UnstructuredStart)
	}
	if !sig.Scored() {
		t.Fatalf("session should be scored; got %+v", sig)
	}
	if h, err := st.PromptHygiene(ctx, store.AnalyticsFilter{}); err != nil {
		t.Fatalf("hygiene aggregate (current): %v", err)
	} else if h.Sessions != 1 || h.Short != 1 || !h.HasData() {
		t.Fatalf("current-version aggregate = %+v, want it to include the session (Sessions 1, Short 1)", h)
	}

	// A ClassifyPrompt change bumps quality.PromptFactsVersion; before the paired reparse re-derives
	// this session, its row still reads the old classifier version. The hygiene must now read as
	// unmeasured on both paths, while the outcome and score stay visible (they do not depend on the
	// classifier, so hiding them would drop correct data).
	setPromptFactsVersion(t, st, ctx, sid, quality.PromptFactsVersion-1)
	stale, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals (stale facts): %v", err)
	}
	if stale.HygieneMeasured || stale.HasHygieneSignal() {
		t.Errorf("stale-classifier hygiene must read unmeasured; got {measured %v, hasSignal %v}", stale.HygieneMeasured, stale.HasHygieneSignal())
	}
	if !stale.Scored() || stale.Outcome != sig.Outcome {
		t.Errorf("stale hygiene must not drop the grade; got scored %v outcome %q, want scored true outcome %q", stale.Scored(), stale.Outcome, sig.Outcome)
	}
	if h, err := st.PromptHygiene(ctx, store.AnalyticsFilter{}); err != nil {
		t.Fatalf("hygiene aggregate (stale facts): %v", err)
	} else if h.Sessions != 0 || h.HasData() {
		t.Errorf("stale-classifier row must drop from the fleet aggregate; got %+v, want no measured session", h)
	}

	// The reparse re-derives the facts and re-grades at the current version. The hygiene is measured
	// and counted again, so the read side hid it only until it was re-derived.
	setPromptFactsVersion(t, st, ctx, sid, quality.PromptFactsVersion)
	back, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals (re-derived): %v", err)
	}
	if !back.HygieneMeasured || !back.HasHygieneSignal() {
		t.Errorf("re-derived hygiene must read measured again; got {measured %v, hasSignal %v}", back.HygieneMeasured, back.HasHygieneSignal())
	}
	if h, err := st.PromptHygiene(ctx, store.AnalyticsFilter{}); err != nil {
		t.Fatalf("hygiene aggregate (re-derived): %v", err)
	} else if h.Sessions != 1 || h.Short != 1 {
		t.Errorf("re-derived aggregate = %+v, want the session back (Sessions 1, Short 1)", h)
	}
}

// TestSessionSignalsByIDVersionFilter pins the per-session read to the running quality
// version: a session that carries only a stale-version row (one a running reparse has not
// yet rewritten) reads as an unknown, unscored result rather than surfacing the old grade,
// so the session header never shows a verdict the Insights aggregates (which count only
// current-version rows) treat as unscored. Once the row is at the current version, the read
// returns it verbatim.
func TestSessionSignalsByIDVersionFilter(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-stale-version")

	// A stale row with a real grade. The version does not match the running one, so the read
	// must ignore it rather than hand back the 'C'.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, score, grade)
		 VALUES ($1, $2, 'completed', 'high', 42, 'C')`,
		sid, quality.Version+999); err != nil {
		t.Fatalf("insert stale signal: %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals (stale row present): %v", err)
	}
	if sig.Scored() || sig.Outcome != string(quality.OutcomeUnknown) || sig.OutcomeConfidence != string(quality.ConfLow) {
		t.Errorf("stale-only read = (%s, %s, scored=%v), want (unknown, low, false); a stale grade must not surface",
			sig.Outcome, sig.OutcomeConfidence, sig.Scored())
	}

	// Re-stamp the row at the current version and clear signals_stale, as a reparse that grades
	// a settled session would. Now the read returns it.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE session_signals SET signals_version = $2 WHERE session_id = $1", sid, quality.Version); err != nil {
		t.Fatalf("restamp signal: %v", err)
	}
	markSignalsFresh(t, st, ctx, sid)
	sig, err = st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals (current row): %v", err)
	}
	if !sig.Scored() || *sig.Score != 42 || *sig.Grade != "C" || sig.Outcome != string(quality.OutcomeCompleted) {
		t.Errorf("current-version read = (%s, score %v, grade %v), want (completed, 42, C)", sig.Outcome, sig.Score, sig.Grade)
	}
}

func signalsRowCount(t *testing.T, st *store.Store, ctx context.Context, sid int64) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM session_signals WHERE session_id = $1", sid).Scan(&n); err != nil {
		t.Fatalf("count signals rows: %v", err)
	}
	return n
}
