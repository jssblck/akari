package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedTranscript inserts one session with msgCount messages (ordinals 0..n-1) owned
// by a freshly named user, and returns its id, so the keyset window can be paged
// against a known transcript. The username distinguishes the owner so a test can call
// this more than once against the same store without colliding on the unique name.
func seedTranscript(t *testing.T, st *store.Store, username string, msgCount int) int64 {
	t.Helper()
	ctx := context.Background()
	uid := seedUser(t, st, username)
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1,$2,'claude','src-1','box') RETURNING id`, uid, pid).Scan(&sid); err != nil {
		t.Fatalf("session: %v", err)
	}
	for i := 0; i < msgCount; i++ {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1,$2,'user','m')`,
			sid, i); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	return sid
}

// TestMessagesAfterKeysetWindows proves the keyset transcript reader walks a whole
// session window by window: every ordinal appears exactly once, in order, with no
// overlap at the page seams, regardless of where the resume cursor lands.
func TestMessagesAfterKeysetWindows(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const total = 25
	sid := seedTranscript(t, st, "grace", total)

	var after *int
	var got []int
	for page := 0; page < 100; page++ {
		// Peek one past the window, exactly as loadTranscript does, so has_more logic is
		// exercised here too.
		const window = 4
		msgs, err := st.MessagesAfter(ctx, sid, after, window+1)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		hasMore := len(msgs) > window
		if hasMore {
			msgs = msgs[:window]
		}
		for _, m := range msgs {
			got = append(got, m.Ordinal)
		}
		if !hasMore {
			break
		}
		last := msgs[len(msgs)-1].Ordinal
		after = &last
	}

	if len(got) != total {
		t.Fatalf("keyset walk returned %d ordinals, want %d: %v", len(got), total, got)
	}
	for i, ord := range got {
		if ord != i {
			t.Fatalf("ordinal at position %d is %d, want %d (gap or overlap): %v", i, ord, i, got)
		}
	}
}

// TestSessionRawToPinsTotalToChunks pins total_bytes to the chunk stream the reader
// serves. A stale session_raw.byte_len (here deliberately wrong) must not leak into
// total: the reported length is the sum of the chunk content actually streamed, so
// total_bytes and content can never disagree.
func TestSessionRawToPinsTotalToChunks(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedTranscript(t, st, "grace", 1)

	raw := []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n")
	// byte_len is intentionally wrong: the reader must ignore it for total.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_raw (session_id, byte_len, content_sha256) VALUES ($1, 9999, $2)`,
		sid, store.HashString(string(raw))); err != nil {
		t.Fatalf("session_raw: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_raw_chunks (session_id, byte_offset, byte_len, content) VALUES ($1,0,$2,$3)`,
		sid, len(raw), raw); err != nil {
		t.Fatalf("session_raw_chunks: %v", err)
	}

	var buf bytes.Buffer
	written, truncated, total, err := st.SessionRawTo(ctx, &buf, sid, 0)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if total != int64(len(raw)) {
		t.Fatalf("total_bytes = %d, want %d (must track chunk content, not byte_len)", total, len(raw))
	}
	if written != int64(len(raw)) || truncated || !bytes.Equal(buf.Bytes(), raw) {
		t.Fatalf("stream mismatch: written=%d truncated=%v content=%q", written, truncated, buf.Bytes())
	}

	// A session that never received an upload is ErrNotFound, not a zero-length read.
	missing := seedTranscript(t, st, "ada", 0)
	if _, _, _, err := st.SessionRawTo(ctx, &bytes.Buffer{}, missing, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing raw: want ErrNotFound, got %v", err)
	}
}
