package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/jssblck/akari/migrations"
)

// newTestStore connects to AKARI_TEST_DATABASE_URL, resets the schema, and
// applies migrations. Tests are skipped when the env var is unset.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AKARI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AKARI_TEST_DATABASE_URL to run store integration tests")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, q := range []string{"DROP SCHEMA public CASCADE", "CREATE SCHEMA public"} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset schema (%s): %v", q, err)
		}
	}
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestRegisterFirstAdminThenInvite(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register first user: %v", err)
	}
	if !admin.IsAdmin {
		t.Fatal("first user should be admin")
	}

	// Second registration without an invite must fail.
	if _, err := st.Register(ctx, "ada", "hash2", ""); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("want ErrInvalidInvite, got %v", err)
	}

	inviteHash := hashHex("invite-secret")
	if _, err := st.CreateInvite(ctx, inviteHash, admin.ID, "for ada", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	ada, err := st.Register(ctx, "ada", "hash2", inviteHash)
	if err != nil {
		t.Fatalf("register with invite: %v", err)
	}
	if ada.IsAdmin {
		t.Fatal("invited user should not be admin")
	}

	// The invite is single use.
	if _, err := st.Register(ctx, "anna", "hash3", inviteHash); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("reused invite: want ErrInvalidInvite, got %v", err)
	}
}

func TestTokenAuth(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := hashHex("token-secret")
	id, err := st.CreateAPIToken(ctx, u.ID, "laptop", "ingest", tokenHash)
	if err != nil {
		t.Fatal(err)
	}

	uid, scope, err := st.TokenAuth(ctx, tokenHash)
	if err != nil {
		t.Fatalf("token auth: %v", err)
	}
	if uid != u.ID || scope != "ingest" {
		t.Fatalf("got (%d,%s), want (%d,ingest)", uid, scope, u.ID)
	}

	if err := st.RevokeAPIToken(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TokenAuth(ctx, tokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token: want ErrNotFound, got %v", err)
	}
}

func TestIngestFlow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	announce := func() AnnounceResult {
		r, err := st.Announce(ctx, AnnounceParams{
			UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
			ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
		})
		if err != nil {
			t.Fatalf("announce: %v", err)
		}
		return r
	}

	first := announce()
	if first.StoredBytes != 0 {
		t.Fatalf("fresh session stored_bytes = %d, want 0", first.StoredBytes)
	}
	if first.PrefixSHA256 != hashHex("") {
		t.Fatalf("empty prefix hash = %q, want sha256 of empty", first.PrefixSHA256)
	}

	// Append two newline-terminated lines.
	line1 := []byte(`{"type":"session","cwd":"/home/grace/akari"}` + "\n")
	stored, err := st.AppendChunk(ctx, first.SessionID, 0, line1)
	if err != nil {
		t.Fatalf("append line1: %v", err)
	}
	if stored != int64(len(line1)) {
		t.Fatalf("stored = %d, want %d", stored, len(line1))
	}

	// Re-announce returns the new cursor and the matching content hash.
	second := announce()
	if second.StoredBytes != int64(len(line1)) {
		t.Fatalf("stored_bytes = %d, want %d", second.StoredBytes, len(line1))
	}
	if second.PrefixSHA256 != hashHexBytes(line1) {
		t.Fatalf("prefix hash mismatch: %q vs %q", second.PrefixSHA256, hashHexBytes(line1))
	}

	// An append at the wrong offset is rejected with the true cursor.
	_, err = st.AppendChunk(ctx, first.SessionID, 0, []byte("x\n"))
	var mismatch OffsetMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want OffsetMismatchError, got %v", err)
	}
	if mismatch.StoredBytes != int64(len(line1)) {
		t.Fatalf("mismatch.StoredBytes = %d, want %d", mismatch.StoredBytes, len(line1))
	}

	// Append the next line at the correct offset.
	line2 := []byte(`{"type":"message"}` + "\n")
	stored, err = st.AppendChunk(ctx, first.SessionID, int64(len(line1)), line2)
	if err != nil {
		t.Fatalf("append line2: %v", err)
	}
	if stored != int64(len(line1)+len(line2)) {
		t.Fatalf("stored = %d, want %d", stored, len(line1)+len(line2))
	}
	if got := hashHexBytes(append(append([]byte{}, line1...), line2...)); announce().PrefixSHA256 != got {
		t.Fatalf("combined prefix hash mismatch")
	}

	// Reset clears the raw store.
	if err := st.ResetRaw(ctx, first.SessionID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if r := announce(); r.StoredBytes != 0 || r.PrefixSHA256 != hashHex("") {
		t.Fatalf("after reset: stored=%d prefix=%q", r.StoredBytes, r.PrefixSHA256)
	}
}

// TestWriteProjectionStaleGuard confirms a projection parsed from a stale raw
// length is rejected, so an older parse cannot clobber a newer one.
func TestWriteProjectionStaleGuard(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-stale", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("line one\nline two\n")
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, raw); err != nil {
		t.Fatal(err)
	}

	p := Projection{MessageCount: 1, ParserVersion: 1, Messages: []ProjMessage{{Ordinal: 0, Role: "user", Content: "x"}}}

	// A write whose rawBytes lags the stored length is stale and must be refused.
	if err := st.WriteProjection(ctx, ann.SessionID, int64(len(raw))-1, p); !errors.Is(err, ErrStaleProjection) {
		t.Fatalf("stale write: want ErrStaleProjection, got %v", err)
	}
	var count int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", ann.SessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale write left %d message rows, want 0", count)
	}

	// A write matching the stored length succeeds.
	if err := st.WriteProjection(ctx, ann.SessionID, int64(len(raw)), p); err != nil {
		t.Fatalf("matching write: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", ann.SessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("matching write left %d message rows, want 1", count)
	}
}

// TestUpsertProjectKindTransition confirms a standalone folder that is later
// deleted transitions to orphaned in place: same key, same row, updated kind.
func TestUpsertProjectKindTransition(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	key := "local:laptop:/home/grace/scratch"
	id1, err := st.UpsertProject(ctx, key, "laptop", "", "scratch", "scratch", "standalone")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.UpsertProject(ctx, key, "laptop", "", "scratch", "scratch", "orphaned")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("transition forked the project row: %d vs %d", id1, id2)
	}
	var kind string
	if err := st.Pool.QueryRow(ctx, "SELECT kind FROM projects WHERE id = $1", id1).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "orphaned" {
		t.Fatalf("kind = %q, want orphaned", kind)
	}
}

// TestAnnounceKeepsRemoteAttribution confirms remote attribution is sticky: a
// session resolved to a git remote stays there even when a later announce can no
// longer find one (its folder was deleted), and an upgrade in the other
// direction (gaining a remote) does re-home the session.
func TestAnnounceKeepsRemoteAttribution(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	remoteID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	localID, err := st.UpsertProject(ctx, "local:laptop:/home/grace/akari", "laptop", "", "akari", "akari", "orphaned")
	if err != nil {
		t.Fatal(err)
	}

	first, err := st.Announce(ctx, AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: remoteID, Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The folder is deleted; the client now announces it as orphaned under a local
	// project. The session must keep its remote attribution and identity.
	second, err := st.Announce(ctx, AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: localID, Kind: "orphaned", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("session id changed: %d vs %d", second.SessionID, first.SessionID)
	}
	var projectID int64
	if err := st.Pool.QueryRow(ctx, "SELECT project_id FROM sessions WHERE id = $1", first.SessionID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if projectID != remoteID {
		t.Fatalf("session moved to project %d, want remote %d (attribution not sticky)", projectID, remoteID)
	}

	// The reverse is allowed: regaining a remote re-homes the session.
	third, err := st.Announce(ctx, AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: remoteID, Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.SessionID != first.SessionID {
		t.Fatalf("session id changed on re-home: %d vs %d", third.SessionID, first.SessionID)
	}

	// A standalone session that never had a remote lands in its local project.
	localOnly, err := st.Announce(ctx, AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-2",
		ProjectID: localID, Kind: "standalone", Cwd: "/home/grace/scratch", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT project_id FROM sessions WHERE id = $1", localOnly.SessionID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if projectID != localID {
		t.Fatalf("standalone session landed in project %d, want local %d", projectID, localID)
	}
}

func hashHex(s string) string      { return hashHexBytes([]byte(s)) }
func hashHexBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
