package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestKeptRemoteAnnounceRefreshesMutableMetadata keeps the remote project while
// applying every mutable field from a later local-classification announce. The
// cwd change must also re-key the stored relative path and churn rollup because
// an announce alone does not schedule a parse rebuild.
func TestKeptRemoteAnnounceRefreshesMutableMetadata(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "anna_winlock", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	remoteID, err := st.UpsertProject(ctx, "github.com/anna/akari", "github.com", "anna", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sticky-metadata",
		ProjectID: remoteID, Kind: "remote", Machine: "old-host",
		Cwd: "/worktree/old", GitBranch: "old-branch", Terminal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/worktree/new/internal/db.go"
	if err := st.RebuildSession(ctx, ann.SessionID, testEpoch, stubReducer{store.ProjectionDelta{
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
			FilePath: path, CallUID: "edit-1",
		}},
	}}); err != nil {
		t.Fatalf("seed projection: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET updated_at = '2000-01-01T00:00:00Z' WHERE id = $1`, ann.SessionID); err != nil {
		t.Fatal(err)
	}

	got, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sticky-metadata",
		Kind: "orphaned", Machine: "new-host", Cwd: "/worktree/new",
		GitBranch: "new-branch", Terminal: false,
	}, store.ProjectParams{
		RemoteKey: "local:new-host:/worktree/new", Host: "new-host",
		Repo: "new", DisplayName: "new", Kind: "orphaned",
	})
	if err != nil {
		t.Fatalf("kept-remote announce: %v", err)
	}
	if got.SessionID != ann.SessionID {
		t.Fatalf("session id = %d, want %d", got.SessionID, ann.SessionID)
	}

	var projectID int64
	var machine, cwd, branch string
	var terminal bool
	var updated time.Time
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id, machine, cwd, git_branch, terminal, updated_at
		   FROM sessions WHERE id = $1`, ann.SessionID).
		Scan(&projectID, &machine, &cwd, &branch, &terminal, &updated); err != nil {
		t.Fatal(err)
	}
	if projectID != remoteID {
		t.Errorf("project_id = %d, want sticky remote %d", projectID, remoteID)
	}
	if machine != "new-host" || cwd != "/worktree/new" || branch != "new-branch" {
		t.Errorf("metadata = (%q, %q, %q), want latest announce", machine, cwd, branch)
	}
	if !terminal {
		t.Error("terminal flag was cleared by a later non-terminal announce")
	}
	if !updated.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("updated_at = %s, want refreshed timestamp", updated)
	}

	var relPath, churnPath string
	if err := st.Pool.QueryRow(ctx,
		`SELECT file_rel_path FROM tool_calls
		  WHERE session_id = $1 AND message_ordinal = 0 AND call_index = 0`, ann.SessionID).Scan(&relPath); err != nil {
		t.Fatalf("read recomputed relative path: %v", err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT churn_path FROM session_file_churn WHERE session_id = $1`, ann.SessionID).Scan(&churnPath); err != nil {
		t.Fatalf("read recomputed churn key: %v", err)
	}
	if relPath != "internal/db.go" || churnPath != "internal/db.go" {
		t.Errorf("derived paths = (%q, %q), want internal/db.go", relPath, churnPath)
	}
}
