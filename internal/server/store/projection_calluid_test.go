package store

import (
	"context"
	"testing"
)

// TestApplyDeltaDuplicateCallUIDBackPatchesEveryCopy exercises a call_uid collision
// that lands inside one delta (one transaction), which the cross-region parse test
// cannot reach: there the first occurrence is already committed when the duplicate
// arrives, here both rows insert under the same ApplyProjectionDelta. With the
// (session_id, call_uid) index non-unique (migration 0010) both rows keep the id and
// the back-patch UPDATE ... WHERE call_uid = $1 stamps the result onto each, so every
// visible copy of a replayed turn carries its result. A third call carries no id at
// all, which stays NULL and unpatched.
func TestApplyDeltaDuplicateCallUIDBackPatchesEveryCopy(t *testing.T) {
	st := newTestStore(t)
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
	delta := ProjectionDelta{
		Messages: []MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "first", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", Content: "replay", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", Content: "unkeyed", HasToolUse: true},
		},
		ToolCalls: []ProjToolCall{
			// Two calls share id "dup": the second is the replayed turn. A third call
			// carries no id at all, exercising the NULL call_uid path.
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Bash", CallUID: ""},
		},
		ToolResults: []ToolResultDelta{
			{CallUID: "dup", Body: string(body), Bytes: int64(len(body)), MediaType: "text/plain", Status: "ok"},
		},
	}
	// The whole delta applies in one transaction; the duplicate id no longer aborts
	// on the index.
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

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
}
