package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func TestSessionEventStreamSignalsEveryConnectionImmediately(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	client := registerAdmin(t, srv.URL)
	ctx := context.Background()
	owner, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("load owner: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sse-reconnect",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	connect := func(attempt int) {
		t.Helper()
		requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet,
			fmt.Sprintf("%s/sessions/%d/events", srv.URL, ann.SessionID), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("connect %d: %v", attempt, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("connect %d status = %d, want 200", attempt, resp.StatusCode)
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: update" {
				return
			}
		}
		t.Fatalf("connect %d ended before initial update: %v", attempt, scanner.Err())
	}

	connect(1)
	connect(2)
}
