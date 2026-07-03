package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestListInvites covers the three statuses the account page renders: unused,
// redeemed (joined to the redeemer's username), and expired. It seeds one of
// each and checks ListInvites reports all three, newest first.
func TestListInvites(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}

	// Unused invite.
	unusedHash := hashHex("unused-invite")
	unusedID, err := st.CreateInvite(ctx, unusedHash, admin.ID, "for ada", nil)
	if err != nil {
		t.Fatalf("create unused invite: %v", err)
	}

	// Redeemed invite: create, then register a second user against it.
	redeemedHash := hashHex("redeemed-invite")
	if _, err := st.CreateInvite(ctx, redeemedHash, admin.ID, "for anna", nil); err != nil {
		t.Fatalf("create redeemed invite: %v", err)
	}
	if _, err := st.Register(ctx, "anna", "hash2", redeemedHash); err != nil {
		t.Fatalf("register anna: %v", err)
	}

	// Expired invite: expires_at in the past, never redeemed.
	expiredHash := hashHex("expired-invite")
	past := time.Now().Add(-time.Hour)
	expiredID, err := st.CreateInvite(ctx, expiredHash, admin.ID, "", &past)
	if err != nil {
		t.Fatalf("create expired invite: %v", err)
	}

	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 3 {
		t.Fatalf("list invites: got %d, want 3", len(invites))
	}

	byID := make(map[int64]store.Invite, len(invites))
	for _, inv := range invites {
		byID[inv.ID] = inv
	}

	unused, ok := byID[unusedID]
	if !ok {
		t.Fatal("unused invite missing from list")
	}
	if unused.Note != "for ada" || unused.CreatedBy != "grace" {
		t.Fatalf("unused invite = %+v, want note %q created by grace", unused, "for ada")
	}
	if unused.RedeemedBy != nil || unused.ExpiresAt != nil {
		t.Fatalf("unused invite should carry no redemption or expiry, got %+v", unused)
	}

	var redeemed store.Invite
	found := false
	for _, inv := range invites {
		if inv.RedeemedBy != nil {
			redeemed = inv
			found = true
		}
	}
	if !found {
		t.Fatal("redeemed invite missing from list")
	}
	if redeemed.RedeemedBy == nil || *redeemed.RedeemedBy != "anna" {
		t.Fatalf("redeemed invite RedeemedBy = %v, want anna", redeemed.RedeemedBy)
	}
	if redeemed.RedeemedAt == nil {
		t.Fatal("redeemed invite should carry a redeemed_at")
	}

	expired, ok := byID[expiredID]
	if !ok {
		t.Fatal("expired invite missing from list")
	}
	if expired.ExpiresAt == nil || !expired.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expired invite ExpiresAt = %v, want a past time", expired.ExpiresAt)
	}
	if expired.RedeemedBy != nil {
		t.Fatalf("expired invite should be unredeemed, got %+v", expired)
	}
}

// TestRevokeInvite checks that revoking removes the invite from ListInvites and
// that the token can no longer be redeemed, and that revoking an id that does
// not exist is a harmless no-op.
func TestRevokeInvite(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}

	tokenHash := hashHex("revoke-me")
	id, err := st.CreateInvite(ctx, tokenHash, admin.ID, "", nil)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	if err := st.RevokeInvite(ctx, id); err != nil {
		t.Fatalf("revoke invite: %v", err)
	}

	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	for _, inv := range invites {
		if inv.ID == id {
			t.Fatalf("revoked invite %d still listed", id)
		}
	}

	if _, err := st.Register(ctx, "ada", "hash2", tokenHash); !errors.Is(err, store.ErrInvalidInvite) {
		t.Fatalf("register with revoked invite: want ErrInvalidInvite, got %v", err)
	}

	// Revoking an unknown id is a no-op, not an error.
	if err := st.RevokeInvite(ctx, 999999); err != nil {
		t.Fatalf("revoke unknown invite: %v", err)
	}
}

// TestRevokeInviteLeavesRedeemedInvite pins the race-closing guard: RevokeInvite only
// deletes UNREDEEMED invites, so an invite already redeemed by a registration is left
// intact (its redemption history stays joinable) rather than deleted out from under the
// account it created. This is the store-side half of the RevokeInvite/Register race
// fix: the delete and the redemption are mutually exclusive on the same row.
func TestRevokeInviteLeavesRedeemedInvite(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	tokenHash := hashHex("redeem-then-revoke")
	id, err := st.CreateInvite(ctx, tokenHash, admin.ID, "", nil)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	// Redeem it by registering a new account with the token.
	if _, err := st.Register(ctx, "ada", "hash2", tokenHash); err != nil {
		t.Fatalf("redeem invite: %v", err)
	}

	// Revoking the now-redeemed invite is a harmless no-op: the delete's redeemed_at IS
	// NULL guard matches nothing, so the redeemed row survives for ListInvites to join.
	if err := st.RevokeInvite(ctx, id); err != nil {
		t.Fatalf("revoke redeemed invite: %v", err)
	}
	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	var found bool
	for _, inv := range invites {
		if inv.ID == id {
			found = true
			if inv.RedeemedBy == nil || *inv.RedeemedBy != "ada" {
				t.Errorf("redeemed invite should still name its redeemer, got %+v", inv)
			}
		}
	}
	if !found {
		t.Error("a redeemed invite must not be deleted by RevokeInvite (it keeps its history)")
	}
}

// TestRevokeInviteLeavesExpiredInvite pins that the write path shares the view's
// revocability policy: an expired (but unredeemed) invite is not revocable, so
// RevokeInvite is a no-op on it, matching classifyInvite marking it "expired · not
// revocable" and Register refusing to redeem it. One policy across read, write, and
// redemption.
func TestRevokeInviteLeavesExpiredInvite(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "hash1", "")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	// An invite that expired an hour ago (unredeemed): past its expiry, so no longer
	// open. CreateInvite takes an optional expiry; pin one in the past.
	past := time.Now().Add(-time.Hour)
	id, err := st.CreateInvite(ctx, hashHex("expired-open"), admin.ID, "", &past)
	if err != nil {
		t.Fatalf("create expired invite: %v", err)
	}

	// Revoking is a no-op: the expiry guard matches nothing, so the row survives (its
	// absence of a Revoke button in the UI and this no-op are the same policy).
	if err := st.RevokeInvite(ctx, id); err != nil {
		t.Fatalf("revoke expired invite: %v", err)
	}
	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	var found bool
	for _, inv := range invites {
		if inv.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("an expired invite must not be deleted by RevokeInvite (it is not revocable)")
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

// TestResetRawClearsModelFallbacks pins that ResetRaw clears the model_fallbacks projection
// rows alongside zeroing sessions.model_fallback_count, so the invariant
// model_fallback_count == count(model_fallbacks) holds after a reset. Leaving the rows behind
// would both diverge the count from the rollup and let a re-upload of the same raw merge into
// the stale (session_id, dedup_key) rows instead of inserting, so the re-count never fires.
func TestResetRawClearsModelFallbacks(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/ada/reset", "github.com", "ada", "reset", "reset", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, pid, "reset-fb")

	// A chunk gives the session a session_raw row (ResetRaw is a no-op without one).
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(`{"type":"session"}`+"\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Model the post-parse projection state directly: two fallback rows and the matching rollup.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger, occurred_at, dedup_key)
		 VALUES ($1, 1, 'claude-fable-5', 'claude-opus-4-8', 'refusal', now(), 'req-a'),
		        ($1, 2, 'claude-fable-5', 'claude-opus-4-8', 'refusal', now(), 'req-b')`, sid); err != nil {
		t.Fatalf("seed fallbacks: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET model_fallback_count = 2 WHERE id = $1", sid); err != nil {
		t.Fatalf("stamp rollup: %v", err)
	}

	if err := st.ResetRaw(ctx, sid); err != nil {
		t.Fatalf("reset: %v", err)
	}

	var rows, count int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM model_fallbacks WHERE session_id = $1", sid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT model_fallback_count FROM sessions WHERE id = $1", sid).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("model_fallbacks rows after reset = %d, want 0 (the reset must delete them)", rows)
	}
	if count != 0 {
		t.Errorf("model_fallback_count after reset = %d, want 0", count)
	}
	if rows != count {
		t.Errorf("invariant broken: count(model_fallbacks)=%d != model_fallback_count=%d", rows, count)
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

func TestAnnounceWithProjectSkipsUnusedLocalDowngradeProject(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
	}, store.ProjectParams{
		RemoteKey: "github.com/jssblck/akari", Host: "github.com", Owner: "jssblck",
		Repo: "akari", DisplayName: "akari", Kind: "remote",
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte("line one\n")
	if _, err := st.AppendChunk(ctx, first.SessionID, 0, chunk); err != nil {
		t.Fatal(err)
	}

	second, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		Kind: "orphaned", Cwd: "/home/grace/akari", Machine: "laptop",
	}, store.ProjectParams{
		RemoteKey: "local:laptop:/home/grace/akari", Host: "laptop",
		Repo: "akari", DisplayName: "akari", Kind: "orphaned",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("session id changed: %d vs %d", second.SessionID, first.SessionID)
	}
	if second.StoredBytes != int64(len(chunk)) {
		t.Fatalf("sticky downgrade stored bytes = %d, want %d", second.StoredBytes, len(chunk))
	}
	if second.PrefixSHA256 != hashHexBytes(chunk) {
		t.Fatalf("sticky downgrade prefix hash = %q, want %q", second.PrefixSHA256, hashHexBytes(chunk))
	}
	var localProjects int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM projects WHERE remote_key = 'local:laptop:/home/grace/akari'").Scan(&localProjects); err != nil {
		t.Fatal(err)
	}
	if localProjects != 0 {
		t.Fatalf("unused local downgrade projects = %d, want 0", localProjects)
	}
}

func TestAnnounceWithProjectRollsBackProjectUpsertFailure(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-invalid",
		Kind: "remote", Cwd: "/home/grace/bad", Machine: "laptop",
	}, store.ProjectParams{
		RemoteKey: "github.com/jssblck/bad", Host: "github.com", Owner: "jssblck",
		Repo: "bad", DisplayName: "bad", Kind: "invalid-kind",
	})
	if err == nil {
		t.Fatal("announce with invalid project kind succeeded")
	}
	if !strings.Contains(err.Error(), "upsert project for announce") {
		t.Fatalf("error = %q, want project-upsert context", err.Error())
	}
	var sessions int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM sessions WHERE source_session_id = 'sess-invalid'").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("sessions after failed project upsert = %d, want 0", sessions)
	}
}

func TestAnnounceWithProjectSerializesSessionIdentity(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(
			hashtext(current_database() || ':announce-session'),
			hashtext($1::bigint::text || chr(31) || $2 || chr(31) || $3)
		)`,
		u.ID, "claude", "sess-lock"); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
			UserID: u.ID, Agent: "claude", SourceSessionID: "sess-lock",
			Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
		}, store.ProjectParams{
			RemoteKey: "github.com/jssblck/akari", Host: "github.com", Owner: "jssblck",
			Repo: "akari", DisplayName: "akari", Kind: "remote",
		})
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("announce failed while lock was held: %v", err)
		}
		t.Fatal("announce completed while session identity lock was held")
	case <-time.After(100 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("announce after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("announce did not finish after releasing session identity lock")
	}
}

func TestAnnounceWithProjectRollsBackSessionUpsertFailure(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	_, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: 999_999, Agent: "claude", SourceSessionID: "sess-missing-user",
		Kind: "remote", Cwd: "/home/grace/missing", Machine: "laptop",
	}, store.ProjectParams{
		RemoteKey: "github.com/jssblck/missing", Host: "github.com", Owner: "jssblck",
		Repo: "missing", DisplayName: "missing", Kind: "remote",
	})
	if err == nil {
		t.Fatal("announce with missing user succeeded")
	}
	if !strings.Contains(err.Error(), "upsert session for announce") {
		t.Fatalf("error = %q, want session-upsert context", err.Error())
	}
	var projects int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM projects WHERE remote_key = 'github.com/jssblck/missing'").Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 0 {
		t.Fatalf("projects after failed session upsert = %d, want 0", projects)
	}
}

// TestAnnounceTerminalIsSticky pins the persistence rule for the terminal flag: a
// --finalize announce sets it, and a later ordinary re-announce of the same session (a
// watch loop that resyncs the file with terminal=false) must never clear it. The flag
// records that a session ran on an ephemeral host and was closed out deliberately, so
// once true it stays true regardless of subsequent syncs.
func TestAnnounceTerminalIsSticky(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	terminalOf := func() bool {
		t.Helper()
		var terminal bool
		if err := st.Pool.QueryRow(ctx,
			"SELECT terminal FROM sessions WHERE user_id = $1 AND agent = 'claude' AND source_session_id = 'sess-term'",
			u.ID).Scan(&terminal); err != nil {
			t.Fatalf("read terminal: %v", err)
		}
		return terminal
	}

	// A first ordinary announce leaves it false (the default): an ordinary sync never
	// marks a session terminal.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-term", ProjectID: pid,
	}); err != nil {
		t.Fatalf("first announce: %v", err)
	}
	if terminalOf() {
		t.Error("ordinary announce set terminal, want false")
	}

	// A --finalize announce sets it.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-term", ProjectID: pid, Terminal: true,
	}); err != nil {
		t.Fatalf("finalize announce: %v", err)
	}
	if !terminalOf() {
		t.Error("finalize announce did not set terminal")
	}

	// A later ordinary re-announce (terminal=false) must not clear it: the flag is sticky.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-term", ProjectID: pid,
	}); err != nil {
		t.Fatalf("re-announce: %v", err)
	}
	if !terminalOf() {
		t.Error("ordinary re-announce cleared a terminal flag; it must be sticky")
	}
}

// TestAnnounceTerminalOnKeptRemoteSession covers the terminal flag on the sticky-remote
// downgrade path: when a session already resolved to a git remote and a later standalone or
// orphaned --finalize announce is kept on that remote attribution (rather than re-homed to a
// local project), the announce bypasses the main session upsert, so the terminal flag has to be
// persisted on that path too. Otherwise a --finalize sync of a session whose checkout lost its
// remote would upload the transcript but never grade it promptly.
func TestAnnounceTerminalOnKeptRemoteSession(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	// First announce resolves the session to a git-remote project.
	if _, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-kept",
		Kind: "remote", Cwd: "/home/grace/akari", Machine: "laptop",
	}, store.ProjectParams{
		RemoteKey: "github.com/jssblck/akari", Host: "github.com", Owner: "jssblck",
		Repo: "akari", DisplayName: "akari", Kind: "remote",
	}); err != nil {
		t.Fatalf("remote announce: %v", err)
	}

	// A later --finalize sync from a checkout that lost its remote announces standalone with
	// Terminal set. The sticky-remote guard keeps the remote attribution, but the terminal flag
	// must still land.
	if _, err := st.AnnounceWithProject(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-kept",
		Kind: "standalone", Cwd: "/home/grace/akari", Machine: "laptop", Terminal: true,
	}, store.ProjectParams{
		RemoteKey: "local:laptop:/home/grace/akari", Host: "laptop",
		Repo: "akari", DisplayName: "akari", Kind: "standalone",
	}); err != nil {
		t.Fatalf("kept-remote terminal announce: %v", err)
	}

	var terminal bool
	if err := st.Pool.QueryRow(ctx,
		"SELECT terminal FROM sessions WHERE user_id = $1 AND source_session_id = 'sess-kept'", u.ID).Scan(&terminal); err != nil {
		t.Fatalf("read terminal: %v", err)
	}
	if !terminal {
		t.Error("terminal flag not persisted on the kept-remote announce path")
	}
}

func hashHex(s string) string      { return hashHexBytes([]byte(s)) }
func hashHexBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
