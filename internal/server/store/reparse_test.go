package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestReparseSessionIsAtomic confirms a session's reparse is all-or-nothing: a reduce
// failure partway through rolls back the clear, so the session keeps its prior
// projection rather than being left empty, and a success replaces it. This is the
// property the reparse service relies on to tolerate a per-session parser failure
// without data loss and to retry an operational failure safely.
func TestReparseSessionIsAtomic(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid.ID, Agent: "claude", SourceSessionID: "s-atomic", ProjectID: pid,
		GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID
	if _, err := st.AppendChunk(ctx, sid, 0, []byte("one transcript line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}

	// emit ignores the raw and produces a single message with the given content, so the
	// projection content is whatever the most recent successful reduce chose.
	emit := func(content string) store.ReduceFunc {
		return func(_, _ []byte, _ int64) ([]byte, store.ProjectionDelta, error) {
			return []byte("{}"), store.ProjectionDelta{
				Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: content}},
			}, nil
		}
	}
	readMessage := func() (count int, content string) {
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id = $1", sid).Scan(&count); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if count == 1 {
			if err := st.Pool.QueryRow(ctx, "SELECT content FROM messages WHERE session_id = $1", sid).Scan(&content); err != nil {
				t.Fatalf("read message: %v", err)
			}
		}
		return count, content
	}
	parsedLen := func() int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx, "SELECT parsed_byte_len FROM session_raw WHERE session_id = $1", sid).Scan(&n); err != nil {
			t.Fatalf("read cursor: %v", err)
		}
		return n
	}

	// Build the original projection.
	if err := st.ReparseSession(ctx, sid, 3, emit("original")); err != nil {
		t.Fatalf("initial reparse: %v", err)
	}
	if n, c := readMessage(); n != 1 || c != "original" {
		t.Fatalf("after initial reparse got %d message(s) %q, want 1 %q", n, c, "original")
	}
	cursor := parsedLen()
	if cursor == 0 {
		t.Fatal("cursor should have advanced past zero after the initial reparse")
	}

	// A reduce that fails mid-replay must leave the original projection untouched.
	boom := errors.New("operational failure mid-replay")
	failing := func(_, _ []byte, _ int64) ([]byte, store.ProjectionDelta, error) {
		return nil, store.ProjectionDelta{}, boom
	}
	if err := st.ReparseSession(ctx, sid, 3, failing); !errors.Is(err, boom) {
		t.Fatalf("failing reparse error = %v, want it to wrap boom", err)
	}
	if n, c := readMessage(); n != 1 || c != "original" {
		t.Fatalf("after a failed reparse got %d message(s) %q, want the original 1 %q preserved", n, c, "original")
	}
	if got := parsedLen(); got != cursor {
		t.Fatalf("a failed reparse moved the cursor to %d, want it left at %d", got, cursor)
	}

	// A successful reparse replaces the projection.
	if err := st.ReparseSession(ctx, sid, 3, emit("rebuilt")); err != nil {
		t.Fatalf("second reparse: %v", err)
	}
	if n, c := readMessage(); n != 1 || c != "rebuilt" {
		t.Fatalf("after a successful reparse got %d message(s) %q, want 1 %q", n, c, "rebuilt")
	}
}

// TestReparsedEpochRoundTrip confirms a fresh database reports epoch 0 (so the
// server treats its corpus as needing a reparse and converges) and that a write
// is read back.
func TestReparsedEpochRoundTrip(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	got, err := st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if got != 0 {
		t.Fatalf("fresh database epoch = %d, want 0", got)
	}

	if err := st.SetReparsedEpoch(ctx, 7); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	got, err = st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch after set: %v", err)
	}
	if got != 7 {
		t.Fatalf("epoch after set = %d, want 7", got)
	}

	// The singleton constraint means a second set updates the one row rather than
	// inserting another.
	if err := st.SetReparsedEpoch(ctx, 9); err != nil {
		t.Fatalf("set epoch again: %v", err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM parse_meta").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("parse_meta rows = %d, want 1 (singleton)", rows)
	}
}

// TestAcquireReparseLock confirms the advisory lock is mutually exclusive across
// connections and is reusable after release: this is what keeps two server
// instances from reparsing the same corpus at once. ReparseLockHeld observes the
// same state from any connection, which is how a follower instance gates its
// parsed UI while another instance reparses.
func TestAcquireReparseLock(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	if held, err := st.ReparseLockHeld(ctx); err != nil || held {
		t.Fatalf("lock should be free initially: held=%v err=%v", held, err)
	}

	first, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	if !ok {
		t.Fatal("first acquire should succeed on a free lock")
	}

	if held, err := st.ReparseLockHeld(ctx); err != nil || !held {
		t.Fatalf("lock should read as held while owned: held=%v err=%v", held, err)
	}

	// A second acquire takes a different pooled connection, so the session-scoped
	// advisory lock is already held: it must fail without blocking.
	second, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire second: %v", err)
	}
	if ok {
		second.Release(ctx)
		t.Fatal("second acquire should fail while the lock is held")
	}

	// After release the lock is free again.
	first.Release(ctx)
	if held, err := st.ReparseLockHeld(ctx); err != nil || held {
		t.Fatalf("lock should read as free after release: held=%v err=%v", held, err)
	}
	third, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire third: %v", err)
	}
	if !ok {
		t.Fatal("acquire should succeed after the lock is released")
	}
	third.Release(ctx)
}
