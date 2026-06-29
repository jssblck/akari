package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestRegisterFirstAdminThenInvite(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register first user: %v", err)
	}
	if !admin.IsAdmin {
		t.Fatal("first user should be admin")
	}

	// Second registration without an invite must fail.
	if _, err := st.Register(ctx, "ada", "hash2", ""); !errors.Is(err, store.ErrInvalidInvite) {
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
	if _, err := st.Register(ctx, "anna", "hash3", inviteHash); !errors.Is(err, store.ErrInvalidInvite) {
		t.Fatalf("reused invite: want ErrInvalidInvite, got %v", err)
	}
}

func TestTokenAuth(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
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
	if _, _, err := st.TokenAuth(ctx, tokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token: want ErrNotFound, got %v", err)
	}
}

func TestIngestFlow(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	announce := func() store.AnnounceResult {
		r, err := st.Announce(ctx, store.AnnounceParams{
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
	var mismatch store.OffsetMismatchError
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

	// A chunk that does not end on a newline is rejected and stores nothing: the
	// server only ever holds complete lines.
	if _, err := st.AppendChunk(ctx, first.SessionID, int64(len(line1)+len(line2)), []byte("no newline here")); !errors.Is(err, store.ErrChunkNotLineAligned) {
		t.Fatalf("unterminated chunk: want ErrChunkNotLineAligned, got %v", err)
	}
	if r := announce(); r.StoredBytes != int64(len(line1)+len(line2)) {
		t.Fatalf("rejected chunk changed the cursor to %d", r.StoredBytes)
	}

	// Reset clears the raw store.
	if err := st.ResetRaw(ctx, first.SessionID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if r := announce(); r.StoredBytes != 0 || r.PrefixSHA256 != hashHex("") {
		t.Fatalf("after reset: stored=%d prefix=%q", r.StoredBytes, r.PrefixSHA256)
	}
}

// TestAdvanceProjectionCursorAndVersionGate exercises the incremental applier
// directly with a stub reducer: the parse cursor advances to the stored length
// and folds the delta into the aggregates, a caught-up session is a no-op, and a
// session partially parsed by one version refuses to continue under another.
func TestAdvanceProjectionCursorAndVersionGate(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-adv", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte("line one\nline two\n")); err != nil {
		t.Fatal(err)
	}

	// A stub reducer that emits one user message and one usage row per call, keyed
	// by the region's base offset so repeated calls never collide on the messages
	// primary key or the usage source identity. The rollups are derived from the
	// rows that actually insert, so the delta carries rows, not precomputed counts.
	var calls int
	reduce := func(state, region []byte, base int64) ([]byte, store.ProjectionDelta, error) {
		calls++
		return []byte("{}"), store.ProjectionDelta{
			Messages: []store.MessageDelta{{Ordinal: int(base), Role: "user", Content: "x"}},
			Usage:    []store.ProjUsage{{Model: "m", Input: 5, SourceOffset: base, SourceIndex: 0}},
		}, nil
	}

	parsedTo, caughtUp, err := st.AdvanceProjection(ctx, ann.SessionID, 1, reduce)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !caughtUp || parsedTo != int64(len("line one\nline two\n")) {
		t.Fatalf("advance: caughtUp=%v parsedTo=%d", caughtUp, parsedTo)
	}

	var mc int
	var in int64
	var parsed int64
	if err := st.Pool.QueryRow(ctx, "SELECT message_count, total_input_tokens FROM sessions WHERE id=$1", ann.SessionID).Scan(&mc, &in); err != nil {
		t.Fatal(err)
	}
	if mc != 1 || in != 5 {
		t.Fatalf("aggregates: message_count=%d input=%d, want 1 and 5", mc, in)
	}

	// A second advance with nothing new is a no-op: the reducer is not called and
	// the aggregates do not move.
	before := calls
	if _, caughtUp, err = st.AdvanceProjection(ctx, ann.SessionID, 1, reduce); err != nil || !caughtUp {
		t.Fatalf("advance no-op: caughtUp=%v err=%v", caughtUp, err)
	}
	if calls != before {
		t.Fatalf("reducer ran on a caught-up session: %d calls", calls-before)
	}

	// More bytes arrive, but under a different parser version the partially parsed
	// session refuses to continue until a reparse rewinds it.
	if _, err := st.AppendChunk(ctx, ann.SessionID, int64(len("line one\nline two\n")), []byte("line three\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AdvanceProjection(ctx, ann.SessionID, 2, reduce); !errors.Is(err, store.ErrParserVersionStale) {
		t.Fatalf("version gate: want ErrParserVersionStale, got %v", err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT parsed_byte_len FROM session_raw WHERE session_id=$1", ann.SessionID).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if parsed != int64(len("line one\nline two\n")) {
		t.Fatalf("version gate advanced the cursor to %d", parsed)
	}
}

// TestAdvanceProjectionBatching forces a backlog larger than one parse batch and
// confirms catch-up advances in bounded steps, parsing each chunk's bytes exactly
// once and contiguously (the readRawRegion SQL bound, not a client-side rescan of
// the whole tail).
//
// This test is deliberately not parallel: through the SetParseBatchBytes seam it
// overrides a package-global in store. A non-parallel test runs to completion
// (restoring the global) before any t.Parallel test resumes, so the override
// never races a reader.
func TestAdvanceProjectionBatching(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// A batch smaller than the gap between chunk starts, so each chunk batches alone.
	defer store.SetParseBatchBytes(2)()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-batch", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Three chunks, each a short line, appended before any parse runs: the parse
	// cursor must catch up across several bounded batches.
	chunks := []string{"a\n", "bb\n", "ccc\n"}
	var offset int64
	for _, c := range chunks {
		stored, err := st.AppendChunk(ctx, ann.SessionID, offset, []byte(c))
		if err != nil {
			t.Fatalf("append %q: %v", c, err)
		}
		offset = stored
	}

	var bases []int64
	reduce := func(state, region []byte, base int64) ([]byte, store.ProjectionDelta, error) {
		bases = append(bases, base)
		return []byte("{}"), store.ProjectionDelta{
			Messages: []store.MessageDelta{{Ordinal: int(base), Role: "assistant", Content: string(region)}},
		}, nil
	}

	iterations := 0
	for {
		_, caughtUp, err := st.AdvanceProjection(ctx, ann.SessionID, 1, reduce)
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		iterations++
		if caughtUp {
			break
		}
		if iterations > 10 {
			t.Fatal("catch-up did not converge")
		}
	}

	// One batch per chunk, each starting exactly where the previous ended.
	if len(bases) != 3 {
		t.Fatalf("parsed in %d batches, want 3 (one per chunk): bases=%v", len(bases), bases)
	}
	want := []int64{0, 2, 5}
	for i, b := range bases {
		if b != want[i] {
			t.Fatalf("batch %d started at %d, want %d (bases=%v)", i, b, want[i], bases)
		}
	}

	var mc int
	var parsed, byteLen int64
	if err := st.Pool.QueryRow(ctx, "SELECT message_count FROM sessions WHERE id=$1", ann.SessionID).Scan(&mc); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT parsed_byte_len, byte_len FROM session_raw WHERE session_id=$1", ann.SessionID).Scan(&parsed, &byteLen); err != nil {
		t.Fatal(err)
	}
	if mc != 3 || parsed != byteLen {
		t.Fatalf("after catch-up: message_count=%d parsed=%d byte_len=%d", mc, parsed, byteLen)
	}
}

// TestUpsertProjectKindTransition confirms a standalone folder that is later
// deleted transitions to orphaned in place: same key, same row, updated kind.
func TestUpsertProjectKindTransition(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
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
	t.Parallel()
	st := storetest.NewStore(t)
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

	first, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: remoteID, Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The folder is deleted; the client now announces it as orphaned under a local
	// project. The session must keep its remote attribution and identity.
	second, err := st.Announce(ctx, store.AnnounceParams{
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
	third, err := st.Announce(ctx, store.AnnounceParams{
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
	localOnly, err := st.Announce(ctx, store.AnnounceParams{
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
