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
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
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

func hashHex(s string) string      { return hashHexBytes([]byte(s)) }
func hashHexBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
