package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
)

// TestChunkRejectsUnterminated confirms the ingest endpoint answers 400 for a
// chunk that does not end on a newline and stores nothing, so the line boundary
// the incremental parser relies on is a server-enforced invariant.
func TestChunkRejectsUnterminated(t *testing.T) {
	t.Parallel()
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

// terminalSession is a minimal assistant-only Claude transcript, enough to parse into
// one message and a usage row. An automation-shaped session (no human turn) with a
// substantive assistant last word reads "completed" once it is idle long enough, so it
// is the clean fixture for asserting the terminal shortcut lands a real grade.
const terminalSession = `{"type":"assistant","timestamp":"2024-01-01T10:00:00Z","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"

// TestFinalizeGradesTerminalSession drives the whole server-side --finalize path over
// HTTP: announce the session terminal, upload its transcript, then POST finalize. The
// grade must land immediately even though the session ended moments ago, far short of the
// abandoned-idle window, where an ordinary (non-terminal) session would stay ungraded
// until it settled. This is what makes a CI or sandbox run's grade available before the
// host is torn down.
func TestFinalizeGradesTerminalSession(t *testing.T) {
	t.Parallel()
	srv, st, worker := newTestServerWithReparse(t)
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

	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// Announce terminal, so the server persists sessions.terminal = true.
	resp := do(http.MethodPost, "/api/v1/ingest/session",
		`{"agent":"claude","source_session_id":"sess-final","kind":"remote","project_remote":"github.com/jssblck/akari","cwd":"/home/grace/akari","machine":"laptop","terminal":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("announce status = %d, want 200", resp.StatusCode)
	}
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT id FROM sessions WHERE user_id = $1 AND source_session_id = 'sess-final'", owner.ID).Scan(&sid); err != nil {
		t.Fatalf("look up session: %v", err)
	}
	var terminal bool
	if err := st.Pool.QueryRow(ctx, "SELECT terminal FROM sessions WHERE id = $1", sid).Scan(&terminal); err != nil {
		t.Fatalf("read terminal: %v", err)
	}
	if !terminal {
		t.Fatal("announce did not persist terminal = true")
	}

	// Upload the transcript, then drain the worker so the chunk actually parses into
	// a projection: the chunk handler only appends raw bytes and wakes the worker, it
	// no longer parses inline.
	resp = do(http.MethodPost, fmt.Sprintf("/api/v1/ingest/session/%d/chunk?offset=0", sid), terminalSession)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk status = %d, want 200", resp.StatusCode)
	}
	worker.Drain(ctx)

	// Move the parsed end to now, so the idle window is nowhere near elapsed: only the
	// terminal flag can make this session gradeable. The rebuild the drain just ran
	// stamped ended_at from the fixture's fixed 2024 timestamp and graded inline, so
	// this stamp (and the read below) exercise finalize's own re-grade rather than the
	// rebuild's.
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET ended_at = now(), signals_stale = true WHERE id = $1", sid); err != nil {
		t.Fatalf("set recent ended_at: %v", err)
	}

	// Before finalize the session has no materialized grade, so the read self-heals to
	// unknown: the settle pass has not run and the session is not idle long enough.
	if sig, err := st.SessionSignalsByID(ctx, sid); err != nil {
		t.Fatalf("pre-finalize read: %v", err)
	} else if sig.Scored() {
		t.Fatalf("session graded before finalize (outcome %s); want no grade yet", sig.Outcome)
	}

	// Finalize grades it now.
	resp = do(http.MethodPost, fmt.Sprintf("/api/v1/ingest/session/%d/finalize", sid), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize status = %d, want 200", resp.StatusCode)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("post-finalize read: %v", err)
	}
	if sig.Outcome != string(quality.OutcomeCompleted) {
		t.Errorf("finalized outcome = %s, want completed (automation with a substantive last word)", sig.Outcome)
	}
	if !sig.Scored() {
		t.Error("finalized session is unscored, want a grade")
	}
}

// TestFinalizeRejectsForeignSession confirms the finalize endpoint is owner-scoped like
// the other ingest routes: a principal cannot grade another user's session.
func TestFinalizeRejectsForeignSession(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-owned", ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	// A second user with their own ingest token. The first account is the invite-free
	// bootstrap admin, so a second registration needs an invite the admin mints.
	if _, err := st.CreateInvite(ctx, auth.HashToken("inv1"), owner.ID, "", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	intruder, err := st.Register(ctx, "ada", mustHash(t, "lovelace-1843"), auth.HashToken("inv1"))
	if err != nil {
		t.Fatalf("register intruder: %v", err)
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, intruder.ID, "laptop", "ingest", auth.HashToken(rawToken)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/ingest/session/%d/finalize", srv.URL, ann.SessionID), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-user finalize status = %d, want 403", resp.StatusCode)
	}
}

func TestLocalProjectIdentity(t *testing.T) {
	// Two worktrees of the same local-only repo report the same root, so they must
	// collapse onto one project key and display as the repo folder.
	k1, d1 := localProjectIdentity("grace-laptop", "/home/grace/wt/feature-a", "/home/grace/repo")
	k2, d2 := localProjectIdentity("grace-laptop", "/home/grace/wt/feature-b", "/home/grace/repo")
	if k1 != k2 {
		t.Errorf("worktrees of one repo got different keys: %q vs %q", k1, k2)
	}
	if d1 != "repo" || d2 != "repo" {
		t.Errorf("display names = %q/%q, want repo (the shared root's folder)", d1, d2)
	}
	// Without a root, the key falls back to the per-session cwd, so distinct folders
	// stay distinct (orphaned worktrees, non-git folders, older clients).
	k3, d3 := localProjectIdentity("grace-laptop", "/home/grace/wt/feature-a", "")
	if k3 == k1 {
		t.Error("rootless fallback collapsed onto the grouped key")
	}
	if d3 != "feature-a" {
		t.Errorf("rootless display = %q, want feature-a (the cwd folder)", d3)
	}
	// An empty location still yields a stable, labeled key.
	if _, d := localProjectIdentity("grace-laptop", "", ""); d != "(unknown location)" {
		t.Errorf("empty-location display = %q, want (unknown location)", d)
	}
}

// TestAnnounceGroupsWorktreesByLocalRoot drives the full ingest endpoint: two
// standalone sessions from different worktrees of one local-only repo, both
// reporting the same local_root, must land in a single project keyed on that root
// and displayed as the repo folder. This is the server half of the worktree
// collapse the resolver feeds.
func TestAnnounceGroupsWorktreesByLocalRoot(t *testing.T) {
	t.Parallel()
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

	announce := func(sid, cwd, root string) {
		t.Helper()
		body := fmt.Sprintf(
			`{"agent":"claude","source_session_id":%q,"kind":"standalone","cwd":%q,"local_root":%q,"machine":"grace-laptop"}`,
			sid, cwd, root)
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest/session", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("announce %s: %v", sid, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("announce %s status = %d, want 200", sid, resp.StatusCode)
		}
	}

	announce("wt-a", "/home/grace/wt/feature-a", "/home/grace/repo")
	announce("wt-b", "/home/grace/wt/feature-b", "/home/grace/repo")

	projs, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projs) != 1 {
		t.Fatalf("got %d projects, want 1 (the two worktrees should collapse)", len(projs))
	}
	p := projs[0]
	if p.SessionCount != 2 {
		t.Errorf("session count = %d, want 2", p.SessionCount)
	}
	if p.DisplayName != "repo" {
		t.Errorf("display name = %q, want repo", p.DisplayName)
	}
	if !strings.HasPrefix(p.RemoteKey, "local:") {
		t.Errorf("remote key = %q, want a local: synthetic key", p.RemoteKey)
	}
}

func TestAnnounceLocalDowngradeDoesNotCreateEmptyProject(t *testing.T) {
	t.Parallel()
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

	announce := func(body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest/session", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("announce: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("announce status = %d, want 200", resp.StatusCode)
		}
	}

	announce(`{"agent":"claude","source_session_id":"sess-sticky","kind":"remote","project_remote":"github.com/jssblck/akari","cwd":"/home/grace/akari","machine":"laptop"}`)
	announce(`{"agent":"claude","source_session_id":"sess-sticky","kind":"orphaned","cwd":"/home/grace/akari","machine":"laptop"}`)

	var localProjects int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM projects WHERE remote_key = 'local:laptop:/home/grace/akari'").Scan(&localProjects); err != nil {
		t.Fatal(err)
	}
	if localProjects != 0 {
		t.Fatalf("unused local downgrade projects = %d, want 0", localProjects)
	}

	var projectKey string
	if err := st.Pool.QueryRow(ctx,
		`SELECT p.remote_key
		   FROM sessions s JOIN projects p ON p.id = s.project_id
		  WHERE s.source_session_id = 'sess-sticky'`).Scan(&projectKey); err != nil {
		t.Fatal(err)
	}
	if projectKey != "github.com/jssblck/akari" {
		t.Fatalf("session project = %q, want remote project", projectKey)
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
