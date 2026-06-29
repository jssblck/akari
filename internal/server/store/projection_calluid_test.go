package store

import (
	"context"
	"testing"
)

// TestApplyDeltaDeduplicatesCallUIDWithinTransaction exercises the call_uid CASE
// in applyDelta against a collision that lands inside one delta (one
// transaction), which the cross-region parse test cannot reach: there the first
// occurrence is already committed when the duplicate arrives, here both rows are
// inserted under the same ApplyProjectionDelta. It pins three branches of the
// CASE: a fresh id is kept, a second row with that same id (seen as a sibling
// inserted earlier in this transaction) is stored with call_uid NULL, and a row
// that carries no id is NULL outright. The result then back-patches the one row
// that still owns the id, and the nulled duplicate is left unpatched, the honest
// state for an id that can no longer name a single call.
func TestApplyDeltaDeduplicatesCallUIDWithinTransaction(t *testing.T) {
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
			// carries no id at all, exercising the NULL-input branch of the CASE.
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "dup"},
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Bash", CallUID: ""},
		},
		ToolResults: []ToolResultDelta{
			{CallUID: "dup", Body: string(body), Bytes: int64(len(body)), MediaType: "text/plain", Status: "ok"},
		},
	}
	// The whole delta applies in one transaction, so the duplicate is deduped
	// against its sibling rather than aborting on the unique index.
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	// All three calls persist; exactly one owns the id (the first writer), the other
	// two carry call_uid NULL (the replay and the unkeyed call).
	var total, withUID int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE call_uid IS NOT NULL)
		   FROM tool_calls WHERE session_id=$1`, sid).Scan(&total, &withUID); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("tool_calls rows = %d, want 3", total)
	}
	if withUID != 1 {
		t.Fatalf("rows owning call_uid = %d, want 1 (first writer only)", withUID)
	}

	// First writer wins: the lower ordinal keeps the id, so the result lands on it.
	var winnerOrd int
	var status string
	var bytes int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT message_ordinal, coalesce(result_status,''), coalesce(result_bytes,0)
		   FROM tool_calls WHERE session_id=$1 AND call_uid='dup'`, sid).
		Scan(&winnerOrd, &status, &bytes); err != nil {
		t.Fatal(err)
	}
	if winnerOrd != 0 {
		t.Fatalf("id kept by ordinal %d, want 0 (first writer)", winnerOrd)
	}
	if status != "ok" || bytes != int64(len(body)) {
		t.Fatalf("back-patch on id-owning row: status=%q bytes=%d", status, bytes)
	}

	// The nulled duplicate stays unpatched: its id was surrendered, so the result
	// keyed on that id cannot reach it.
	var dupOrdResult string
	if err := st.Pool.QueryRow(ctx,
		`SELECT coalesce(result_status,'') FROM tool_calls
		   WHERE session_id=$1 AND message_ordinal=1`, sid).Scan(&dupOrdResult); err != nil {
		t.Fatal(err)
	}
	if dupOrdResult != "" {
		t.Fatalf("replayed duplicate should be unpatched, got result_status=%q", dupOrdResult)
	}
}
