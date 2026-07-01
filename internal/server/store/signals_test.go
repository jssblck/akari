package store_test

import (
	"context"
	"testing"

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

// TestSignalsUnknownIsUnscored confirms a session with no human turn (a subagent or an
// automated run) and no tool signal is left unscored: the read returns an unknown
// outcome with nil score and grade, the same restraint the UI shows rather than
// inventing a verdict.
func TestSignalsUnknownIsUnscored(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-unknown")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "automated summary"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
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
// verdict. The next catch-up rebuilds it.
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

func signalsRowCount(t *testing.T, st *store.Store, ctx context.Context, sid int64) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM session_signals WHERE session_id = $1", sid).Scan(&n); err != nil {
		t.Fatalf("count signals rows: %v", err)
	}
	return n
}
