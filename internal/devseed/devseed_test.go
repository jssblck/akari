package devseed

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jssblck/akari/internal/server/auth"
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
	t.Setenv("CLAUDE_PROJECTS_DIR", claude)
	t.Setenv("CODEX_SESSIONS_DIR", t.TempDir())
	t.Setenv("PI_DIR", t.TempDir())
	return claude
}

func TestIngestNoFiles(t *testing.T) {
	isolateDiscoveryRoots(t)
	stats, err := ingest(context.Background(), Options{
		ServerURL:   "http://127.0.0.1:1", // never contacted: there is nothing to upload
		TimeLimit:   5 * time.Second,
		Concurrency: 2,
	}, "tok")
	if err != nil {
		t.Fatalf("ingest with no files should not error: %v", err)
	}
	if stats.discovered != 0 {
		t.Fatalf("discovered = %d, want 0", stats.discovered)
	}
}

func TestIngestSystemicFailureErrors(t *testing.T) {
	claude := isolateDiscoveryRoots(t)

	// Plant one discoverable claude session whose cwd exists, so it resolves and is
	// uploaded rather than skipped. The header only needs cwd.
	header, err := json.Marshal(map[string]string{"cwd": t.TempDir(), "gitBranch": "main"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claude, "session.jsonl"), append(header, '\n'), 0o644); err != nil {
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
