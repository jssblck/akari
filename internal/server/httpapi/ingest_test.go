package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
)

// TestChunkRejectsUnterminated confirms the ingest endpoint answers 400 for a
// chunk that does not end on a newline and stores nothing, so the line boundary
// the incremental parser relies on is a server-enforced invariant.
func TestChunkRejectsUnterminated(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, owner.ID, "laptop", "ingest", auth.HashToken(rawToken)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1", ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	post := func(body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/ingest/session/%d/chunk?offset=0", srv.URL, ann.SessionID), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post chunk: %v", err)
		}
		return resp
	}

	resp := post("no trailing newline")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unterminated chunk status = %d, want 400", resp.StatusCode)
	}
	if r, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1", ProjectID: projectID,
	}); err != nil || r.StoredBytes != 0 {
		t.Fatalf("rejected chunk stored bytes: %d (err=%v)", r.StoredBytes, err)
	}

	// A newline-terminated chunk at the same offset is accepted.
	resp = post("{\"type\":\"user\",\"message\":{\"content\":\"hi\"}}\n")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terminated chunk status = %d, want 200", resp.StatusCode)
	}
}

func TestLocalProjectKey(t *testing.T) {
	// Standalone and orphaned must share a key for the same machine+path so a
	// deleted folder transitions kind in place rather than forking a second row.
	a := localProjectKey("grace-laptop", "/home/grace/scratch")
	b := localProjectKey("grace-laptop", "/home/grace/scratch")
	if a != b {
		t.Fatalf("same machine+path produced different keys: %q vs %q", a, b)
	}
	// Different machine or path must differ.
	if localProjectKey("ada-box", "/home/grace/scratch") == a {
		t.Error("different machine produced same key")
	}
	if localProjectKey("grace-laptop", "/home/grace/other") == a {
		t.Error("different path produced same key")
	}
	// The "local:" prefix keeps synthetic keys out of the remote namespace: a
	// canonicalized git remote ("host/owner/repo") has no colon in its host, so it
	// can never equal a key of this shape.
	if !strings.HasPrefix(a, "local:") {
		t.Errorf("synthetic key %q lacks the local: prefix", a)
	}
}

func TestLastPathSegment(t *testing.T) {
	cases := map[string]string{
		"/home/grace/scratch":     "scratch",
		"/home/grace/scratch/":    "scratch",
		`C:\Users\grace\scratch`:  "scratch",
		`C:\Users\grace\scratch\`: "scratch",
		"scratch":                 "scratch",
		"":                        "",
		"/":                       "",
	}
	for in, want := range cases {
		if got := lastPathSegment(in); got != want {
			t.Errorf("lastPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRemoteKey(t *testing.T) {
	cases := []struct {
		in                string
		host, owner, repo string
		ok                bool
	}{
		{"github.com/jssblck/akari", "github.com", "jssblck", "akari", true},
		{"gitlab.com/group/subgroup/proj", "gitlab.com", "group/subgroup", "proj", true},
		{"github.com/onlyowner", "", "", "", false},
		{"", "", "", "", false},
		{"github.com//repo", "", "", "", false},
		{"/owner/repo", "", "", "", false},
	}
	for _, c := range cases {
		host, owner, repo, ok := parseRemoteKey(c.in)
		if ok != c.ok || host != c.host || owner != c.owner || repo != c.repo {
			t.Errorf("parseRemoteKey(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, host, owner, repo, ok, c.host, c.owner, c.repo, c.ok)
		}
	}
}
