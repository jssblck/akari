package httpapi

import (
	"net/http"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestAPISessionAppend covers the session detail page's live-refresh endpoint: it
// must return only the turns past the given ordinal (not the whole transcript
// again), and it must 404 for a session the store cannot find.
func TestAPISessionAppend(t *testing.T) {
	t.Parallel()
	server, st := newTestServer(t)
	client := registerAdmin(t, server.URL)

	user, err := st.UserByUsername(t.Context(), "grace")
	if err != nil {
		t.Fatalf("load registered user: %v", err)
	}
	projectID, err := st.UpsertProject(t.Context(), "github.com/grace/akari", "github.com", "grace", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	announced, err := st.Announce(t.Context(), store.AnnounceParams{
		UserID: user.ID, Agent: "codex", SourceSessionID: "session-append",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "hopper",
	})
	if err != nil {
		t.Fatalf("announce session: %v", err)
	}
	rebuildWith(t, st, announced.SessionID, store.ProjectionDelta{Messages: []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "first turn"},
		{Ordinal: 1, Role: "assistant", Content: "first reply"},
		{Ordinal: 2, Role: "user", Content: "second turn"},
		{Ordinal: 3, Role: "assistant", Content: "second reply"},
	}})

	sessionPath := server.URL + "/api/v1/app/sessions/" + strconvFormat(announced.SessionID)

	response, appended := doJSON(t, client, http.MethodGet, sessionPath+"/append?after=1", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("append API: status=%d body=%v", response.StatusCode, appended)
	}
	snapshot, ok := appended["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("append API: no snapshot in body=%v", appended)
	}
	page, ok := snapshot["Page"].(map[string]any)
	if !ok {
		t.Fatalf("append API: no page in snapshot=%v", snapshot)
	}
	msgs, _ := page["Msgs"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("append after=1 returned %d messages, want the 2 turns past ordinal 1: %v", len(msgs), msgs)
	}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if ord, _ := msg["Ordinal"].(float64); ord <= 1 {
			t.Fatalf("append after=1 returned a turn at or before the cursor: %v", msg)
		}
	}

	response, badCursor := doJSON(t, client, http.MethodGet, sessionPath+"/append?after=-1", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("append with a negative cursor: status=%d body=%v", response.StatusCode, badCursor)
	}

	response, missing := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/sessions/999999/append?after=0", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("append for a missing session: status=%d body=%v", response.StatusCode, missing)
	}
}
