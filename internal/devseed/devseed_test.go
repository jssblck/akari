package devseed

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/httpapi"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedProject inserts a project row directly and returns its id.
func seedProject(t *testing.T, pool *pgxpool.Pool, remoteKey string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO projects (remote_key, host, owner, repo, display_name)
		 VALUES ($1, 'github.com', 'jssblck', 'akari', $1) RETURNING id`, remoteKey).Scan(&id)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// seedSession inserts a bare session owned by userID and returns its id.
func seedSession(t *testing.T, pool *pgxpool.Pool, userID, projectID int64, src string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1, $2, 'claude', $3, 'box') RETURNING id`, userID, projectID, src).Scan(&id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func TestEnsureUsersIdempotent(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	users, err := ensureUsers(ctx, st.Pool, 4, "akari-dev")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	if len(users) != 4 {
		t.Fatalf("got %d users, want 4", len(users))
	}
	if !users[0].Created {
		t.Errorf("first user should be reported as created on a fresh store")
	}

	// The first account is the admin; the rest are not.
	first, err := st.UserByUsername(ctx, users[0].Username)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if !first.IsAdmin {
		t.Errorf("first account %q should be admin", users[0].Username)
	}
	second, err := st.UserByUsername(ctx, users[1].Username)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if second.IsAdmin {
		t.Errorf("second account %q should not be admin", users[1].Username)
	}

	// The stored hash must verify against the shared password.
	if ok, err := auth.VerifyPassword("akari-dev", first.PasswordHash); err != nil || !ok {
		t.Errorf("password should verify for %q (ok=%v err=%v)", users[0].Username, ok, err)
	}

	// A second call reuses the same accounts rather than failing on the unique
	// username, and reports them as pre-existing.
	again, err := ensureUsers(ctx, st.Pool, 4, "akari-dev")
	if err != nil {
		t.Fatalf("ensureUsers (repeat): %v", err)
	}
	for i := range again {
		if again[i].ID != users[i].ID {
			t.Errorf("user %q id changed: %d -> %d", again[i].Username, users[i].ID, again[i].ID)
		}
		if again[i].Created {
			t.Errorf("user %q should be reported as pre-existing on the second run", again[i].Username)
		}
	}
}

func TestEnsureUsersClampsToRoster(t *testing.T) {
	st := storetest.NewStore(t)
	users, err := ensureUsers(context.Background(), st.Pool, len(roster)+5, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	if len(users) != len(roster) {
		t.Fatalf("got %d users, want roster size %d", len(users), len(roster))
	}
}

func TestCountSessions(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	if n, err := countSessions(ctx, st.Pool); err != nil || n != 0 {
		t.Fatalf("empty store: got (%d, %v), want (0, nil)", n, err)
	}

	users, err := ensureUsers(ctx, st.Pool, 1, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	proj := seedProject(t, st.Pool, "github.com/jssblck/akari")
	seedSession(t, st.Pool, users[0].ID, proj, "s1")
	seedSession(t, st.Pool, users[0].ID, proj, "s2")

	if n, err := countSessions(ctx, st.Pool); err != nil || n != 2 {
		t.Fatalf("after two inserts: got (%d, %v), want (2, nil)", n, err)
	}
}

func TestReassignSessions(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	users, err := ensureUsers(ctx, st.Pool, 4, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	proj := seedProject(t, st.Pool, "github.com/jssblck/akari")

	// Everything starts owned by the first account, mimicking a single uploader.
	const total = 40
	sessionIDs := make([]int64, total)
	for i := range sessionIDs {
		sessionIDs[i] = seedSession(t, st.Pool, users[0].ID, proj, srcName(i))
	}

	valid := map[int64]bool{}
	for _, u := range users {
		valid[u.ID] = true
	}

	rng := rand.New(rand.NewSource(1))
	dist, err := reassignSessions(ctx, st.Pool, userIDs(users), rng)
	if err != nil {
		t.Fatalf("reassignSessions: %v", err)
	}
	if got := totalCount(dist); got != total {
		t.Errorf("distribution sums to %d, want %d", got, total)
	}

	// Every session now belongs to one of the demo accounts.
	rows, err := st.Pool.Query(ctx, `SELECT user_id FROM sessions`)
	if err != nil {
		t.Fatalf("query owners: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !valid[uid] {
			t.Errorf("session owned by unexpected user %d", uid)
		}
		seen++
	}
	if seen != total {
		t.Errorf("saw %d sessions, want %d", seen, total)
	}

	// With 40 sessions over 4 accounts the reassignment should genuinely spread,
	// not pile everything on one owner. A fixed seed makes this deterministic.
	if len(dist) < 2 {
		t.Errorf("reassignment used %d accounts, expected it to spread across several", len(dist))
	}
}

// TestReassignSessionsKeepsFamiliesTogether pins that a parent and its subagents land on one
// owner. The link the ingest path sets means a subagent belongs to the same person as its
// orchestrator, so the shuffle groups on the family root rather than moving each session
// independently. Several seeds exercise different owner draws; none may split the family.
func TestReassignSessionsKeepsFamiliesTogether(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	users, err := ensureUsers(ctx, st.Pool, 4, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	proj := seedProject(t, st.Pool, "github.com/jssblck/akari")

	parent := seedSession(t, st.Pool, users[0].ID, proj, "orchestrator")
	child1 := seedSession(t, st.Pool, users[0].ID, proj, "orchestrator/subagents/agent-1")
	child2 := seedSession(t, st.Pool, users[0].ID, proj, "orchestrator/subagents/agent-2")
	linkChild(t, st.Pool, child1, parent)
	linkChild(t, st.Pool, child2, parent)
	// Standalone sessions so the shuffle has other families to spread across.
	for i := 0; i < 10; i++ {
		seedSession(t, st.Pool, users[0].ID, proj, srcName(i))
	}

	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		if _, err := reassignSessions(ctx, st.Pool, userIDs(users), rng); err != nil {
			t.Fatalf("reassignSessions (seed %d): %v", seed, err)
		}
		parentOwner := ownerOf(t, st.Pool, parent)
		if c1 := ownerOf(t, st.Pool, child1); c1 != parentOwner {
			t.Errorf("seed %d: child1 owner %d != parent owner %d", seed, c1, parentOwner)
		}
		if c2 := ownerOf(t, st.Pool, child2); c2 != parentOwner {
			t.Errorf("seed %d: child2 owner %d != parent owner %d", seed, c2, parentOwner)
		}
	}
}

func linkChild(t *testing.T, pool *pgxpool.Pool, child, parent int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET parent_session_id = $1, relationship_type = 'subagent' WHERE id = $2`, parent, child); err != nil {
		t.Fatalf("link child %d to %d: %v", child, parent, err)
	}
}

func ownerOf(t *testing.T, pool *pgxpool.Pool, sid int64) int64 {
	t.Helper()
	var uid int64
	if err := pool.QueryRow(context.Background(), `SELECT user_id FROM sessions WHERE id = $1`, sid).Scan(&uid); err != nil {
		t.Fatalf("owner of %d: %v", sid, err)
	}
	return uid
}

func srcName(i int) string {
	return "src-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

func TestEnsureUsersClampsLowerBound(t *testing.T) {
	st := storetest.NewStore(t)
	users, err := ensureUsers(context.Background(), st.Pool, 0, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("n=0 should clamp to 1 account, got %d", len(users))
	}
}

func TestReassignSessionsRejectsNoUsers(t *testing.T) {
	st := storetest.NewStore(t)
	if _, err := reassignSessions(context.Background(), st.Pool, nil, rand.New(rand.NewSource(1))); err == nil {
		t.Fatal("reassignSessions with no users should return an error")
	}
}

// isolateDiscoveryRoots points all three agents' discovery roots at fresh, empty
// temp dirs via their documented env overrides, so a test sees only the files it
// plants and never this machine's real session logs. It returns the claude root.
func isolateDiscoveryRoots(t *testing.T) string {
	t.Helper()
	claude := t.TempDir()
	piHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(piHome, "agent", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECTS_DIR", claude)
	t.Setenv("CODEX_SESSIONS_DIR", t.TempDir())
	t.Setenv("PI_DIR", piHome)
	return claude
}

func TestIngestNoFiles(t *testing.T) {
	isolateDiscoveryRoots(t)
	for _, limit := range []time.Duration{5 * time.Second, 0} { // exercise both the deadline and no-deadline branches
		stats, err := ingest(context.Background(), Options{
			ServerURL:   "http://127.0.0.1:1", // never contacted: there is nothing to upload
			TimeLimit:   limit,
			Concurrency: 2,
		}, "tok")
		if err != nil {
			t.Fatalf("ingest with no files (limit %s) should not error: %v", limit, err)
		}
		if stats.discovered != 0 {
			t.Fatalf("discovered = %d, want 0 (limit %s)", stats.discovered, limit)
		}
	}
}

func TestIngestSystemicFailureErrors(t *testing.T) {
	claude := isolateDiscoveryRoots(t)

	// Plant one discoverable claude session whose cwd exists, so it resolves and is
	// uploaded rather than skipped. It must carry a real transcript shape (a typed
	// user entry with a message), not just a cwd, or resolve's positive session
	// detection skips it as non-session JSONL.
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"gitBranch":"main","message":{"content":"hi"}}`+"\n", t.TempDir())
	if err := os.WriteFile(filepath.Join(claude, "session.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	// Point at a closed port so every upload fails at announce. With nothing
	// ingested, ingest must surface a contextual error rather than report success.
	stats, err := ingest(context.Background(), Options{
		ServerURL:   "http://127.0.0.1:1",
		TimeLimit:   10 * time.Second,
		Concurrency: 2,
	}, "tok")
	if err == nil {
		t.Fatalf("ingest with all uploads failing should error; stats=%+v", stats)
	}
	if stats.uploaded != 0 {
		t.Errorf("uploaded = %d, want 0", stats.uploaded)
	}
	if stats.failed == 0 {
		t.Errorf("failed = %d, want at least 1", stats.failed)
	}
}

func TestRunRejectsEmptyServerURL(t *testing.T) {
	st := storetest.NewStore(t)
	if err := Run(context.Background(), st, Options{ServerURL: ""}); err == nil {
		t.Fatal("Run with an empty ServerURL should return an error")
	}
}

// plantClaudeSession writes a minimal, valid claude session file under root, with
// cwd recorded in its header so it resolves (rather than being skipped) and a body
// the server accepts (newline-terminated JSON lines).
func plantClaudeSession(t *testing.T, root, name, cwd string) {
	t.Helper()
	lines := []map[string]any{
		{"type": "user", "cwd": cwd, "gitBranch": "main", "message": map[string]any{"content": "hello"}},
		{"type": "assistant", "message": map[string]any{"content": "hi there"}},
	}
	var buf []byte
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal line: %v", err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, name), buf, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

// TestRunEndToEnd drives the whole orchestration against an in-process server:
// ensure accounts, ingest planted local sessions through the real upload/parse
// pipeline, and reassign them. It then asserts the idempotent skip and the
// --force clean-slate re-seed.
func TestRunEndToEnd(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	worker := parse.NewWorker(st, 4, 0)
	srv := httptest.NewServer(httpapi.New(st, config.Server{}, worker).Routes())
	t.Cleanup(srv.Close)

	claude := isolateDiscoveryRoots(t)
	cwd := t.TempDir() // a real directory so sessions resolve as standalone, not orphaned
	const planted = 12
	for i := 0; i < planted; i++ {
		plantClaudeSession(t, claude, srcName(i)+".jsonl", cwd)
	}

	opts := Options{ServerURL: srv.URL, NumUsers: 3, Password: "pw", TimeLimit: 30 * time.Second, Concurrency: 4}
	if err := Run(ctx, st, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Uploads only append raw bytes; the parse worker's rebuild is what the real
	// server's background drain would eventually do. Drain synchronously so the
	// ingested sessions are actually parsed before the assertions below.
	worker.Drain(ctx)

	// All planted sessions ingested and spread only across the demo accounts.
	users, err := ensureUsers(ctx, st.Pool, 3, "pw")
	if err != nil {
		t.Fatalf("ensureUsers: %v", err)
	}
	valid := map[int64]bool{}
	for _, u := range users {
		valid[u.ID] = true
	}
	n, err := countSessions(ctx, st.Pool)
	if err != nil {
		t.Fatalf("countSessions: %v", err)
	}
	if n != planted {
		t.Fatalf("ingested %d sessions, want %d", n, planted)
	}
	assertOwnedBy(t, st.Pool, valid)

	// Idempotent: a second run with sessions present and Force unset must not
	// ingest again or change the session set.
	if err := Run(ctx, st, opts); err != nil {
		t.Fatalf("Run (idempotent): %v", err)
	}
	if n2, _ := countSessions(ctx, st.Pool); n2 != planted {
		t.Fatalf("idempotent run changed session count: %d, want %d", n2, planted)
	}

	// --force clears and re-seeds from a clean slate, yielding the same count with
	// no duplicate-key collision.
	force := opts
	force.Force = true
	if err := Run(ctx, st, force); err != nil {
		t.Fatalf("Run (force): %v", err)
	}
	if n3, _ := countSessions(ctx, st.Pool); n3 != planted {
		t.Fatalf("force run session count: %d, want %d", n3, planted)
	}
	assertOwnedBy(t, st.Pool, valid)
}

func assertOwnedBy(t *testing.T, pool *pgxpool.Pool, valid map[int64]bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT user_id FROM sessions`)
	if err != nil {
		t.Fatalf("query owners: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !valid[uid] {
			t.Errorf("session owned by unexpected user %d", uid)
		}
	}
}
