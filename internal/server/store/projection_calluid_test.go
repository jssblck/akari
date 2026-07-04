package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestApplyDeltaDuplicateCallUIDBackPatchesEveryCopy exercises a call_uid collision:
// a resumed or compacted transcript replays a prior turn verbatim, so the same
// call_uid legitimately rides two rows in one whole-session delta. With the
// (session_id, call_uid) index non-unique (migration 0010) both rows keep the id and
// the fold's result patching stamps the result onto each still-unresolved copy, so
// every visible copy of a replayed turn carries its result. A third call carries no
// id at all, which stays NULL and unpatched.
func TestApplyDeltaDuplicateCallUIDBackPatchesEveryCopy(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "sess-calluid-batch")

	body := []byte("package auth")
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "first", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", Content: "replay", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", Content: "unkeyed", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			// Two calls share id "dup": the second is the replayed turn. A third call
			// carries no id at all, exercising the NULL call_uid path.
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Bash", CallUID: ""},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "dup", Body: string(body), Bytes: int64(len(body)), MediaType: "text/plain", Status: "ok"},
		},
	}
	// The whole delta rebuilds in one transaction; the duplicate id no longer
	// aborts on the index.
	rebuildWith(t, st, sid, delta)

	// All three calls persist; both "dup" rows keep the id, the unkeyed call is NULL.
	var total, withUID, nulls int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE call_uid = 'dup'),
		        count(*) FILTER (WHERE call_uid IS NULL)
		   FROM tool_calls WHERE session_id=$1`, sid).Scan(&total, &withUID, &nulls); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("tool_calls rows = %d, want 3", total)
	}
	if withUID != 2 || nulls != 1 {
		t.Fatalf("call_uid split = (dup %d, null %d), want (2, 1)", withUID, nulls)
	}

	// Both replayed copies of the call carry the same back-patched result.
	var patched int
	var minBytes int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE result_status='ok'), coalesce(min(result_bytes),0)
		   FROM tool_calls WHERE session_id=$1 AND call_uid='dup'`, sid).
		Scan(&patched, &minBytes); err != nil {
		t.Fatal(err)
	}
	if patched != 2 {
		t.Fatalf("dup rows with a result = %d, want 2 (back-patch stamps every copy)", patched)
	}
	if minBytes != int64(len(body)) {
		t.Fatalf("result_bytes = %d on a dup row, want %d", minBytes, len(body))
	}

	// The unkeyed call has no id to match, so it stays pending.
	var unkeyedStatus string
	if err := st.Pool.QueryRow(ctx,
		`SELECT coalesce(result_status,'') FROM tool_calls
		   WHERE session_id=$1 AND call_uid IS NULL`, sid).Scan(&unkeyedStatus); err != nil {
		t.Fatal(err)
	}
	if unkeyedStatus != "" {
		t.Fatalf("unkeyed call should be unpatched, got result_status=%q", unkeyedStatus)
	}

	// The session view's chip reads this scalar: exactly one id ("dup") appears on
	// more than one row, and the unkeyed call is not counted.
	dups, err := st.DuplicateCallUIDCount(ctx, sid)
	if err != nil {
		t.Fatalf("duplicate count: %v", err)
	}
	if dups != 1 {
		t.Fatalf("DuplicateCallUIDCount = %d, want 1", dups)
	}
}

// TestDuplicateCallUIDCountGroups confirms the chip scalar counts colliding ids, not
// rows: zero when every id is distinct, and one per duplicated id otherwise.
func TestDuplicateCallUIDCountGroups(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "anna", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	unique := seedSession(t, st, u.ID, projectID, "sess-calluid-unique")
	rebuildWith(t, st, unique, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "a"},
			{MessageOrdinal: 0, CallIndex: 1, ToolName: "Bash", CallUID: "b"},
		},
	})
	if dups, err := st.DuplicateCallUIDCount(ctx, unique); err != nil || dups != 0 {
		t.Fatalf("DuplicateCallUIDCount(unique) = %d, err=%v; want 0", dups, err)
	}

	// Two ids each repeated, one distinct, plus an unkeyed call: the count is the two
	// colliding ids, not the row total and not the distinct call.
	multi := seedSession(t, st, u.ID, projectID, "sess-calluid-multi")
	rebuildWith(t, st, multi, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "x"},
			{MessageOrdinal: 0, CallIndex: 1, ToolName: "Read", CallUID: "y"},
			{MessageOrdinal: 0, CallIndex: 2, ToolName: "Grep", CallUID: "z"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "x"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", CallUID: "y"},
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Bash", CallUID: ""},
		},
	})
	if dups, err := st.DuplicateCallUIDCount(ctx, multi); err != nil || dups != 2 {
		t.Fatalf("DuplicateCallUIDCount(multi) = %d, err=%v; want 2", dups, err)
	}
}

// TestBackPatchSkipsAlreadyResolvedCopies confirms the first-result-wins guard
// in the whole-session fold: the first result for a duplicated call id resolves
// every copy of that call (a replayed call is the same call, so its copies share
// the result), and a later result for the same id, the replay re-delivering what
// already resolved, is dropped entirely. It must not overwrite the resolved rows
// and must not write a stray blob nothing references.
func TestBackPatchSkipsAlreadyResolvedCopies(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "sess-calluid-replay")

	// A resumed or compacted transcript logs the same call id more than once and
	// reports its result twice too: three copies of call "dup" and, in arrival
	// order, its result twice. The bodies differ here only so the assertions can
	// tell which one landed; in a real replay they are identical.
	first := []byte("first result")
	second := []byte("second result, longer")
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "dup", Body: string(first), Bytes: int64(len(first)), MediaType: "text/plain", Status: "ok"},
			{CallUID: "dup", Body: string(second), Bytes: int64(len(second)), MediaType: "text/plain", Status: "ok"},
		},
	})

	// All three copies carry the first result; the replayed result touched nothing.
	var keptFirst, keptSecond int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE result_bytes = $2),
		        count(*) FILTER (WHERE result_bytes = $3)
		   FROM tool_calls WHERE session_id = $1`,
		sid, int64(len(first)), int64(len(second))).Scan(&keptFirst, &keptSecond); err != nil {
		t.Fatal(err)
	}
	if keptFirst != 3 || keptSecond != 0 {
		t.Fatalf("rows carrying first/second result = %d/%d, want 3/0 (the first result resolves every copy; the replay overwrites none)", keptFirst, keptSecond)
	}

	// The dropped replay stored no blob: with every copy already resolved there is
	// nothing to reference its body, so writing it would orphan bytes in the CAS.
	var strayBlobs int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE sha256 = $1`, store.HashBytes(second)).Scan(&strayBlobs); err != nil {
		t.Fatal(err)
	}
	if strayBlobs != 0 {
		t.Fatalf("the dropped replay result left %d blob(s) in the CAS, want 0", strayBlobs)
	}
}
