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

// TestSessionRawByteLenMatchesChunks pins the invariant SessionRawTo depends on for
// total_bytes: session_raw.byte_len, the O(1) running length AppendChunk and ResetRaw
// maintain, stays equal to the summed length of the chunk content the reader streams.
// If these ever drifted, total_bytes would stop describing the bytes served. The test
// drives the production append path so the maintained value, the chunk sum, and the
// streamed content must all agree, and it exercises the capped read alongside the
// full one.
func TestSessionRawByteLenMatchesChunks(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: "sess-raw",
		ProjectID: pid, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "box",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	// Append several line-aligned chunks through the production path, which updates
	// session_raw.byte_len in the same transaction that writes each chunk row.
	lines := [][]byte{
		[]byte("{\"type\":\"user\"}\n"),
		[]byte("{\"type\":\"assistant\"}\n"),
		[]byte("{\"type\":\"result\"}\n"),
	}
	var whole []byte
	var offset int64
	for _, l := range lines {
		if _, err := st.AppendChunk(ctx, ann.SessionID, offset, l); err != nil {
			t.Fatalf("append at %d: %v", offset, err)
		}
		offset += int64(len(l))
		whole = append(whole, l...)
	}

	var byteLen, chunkSum int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT byte_len FROM session_raw WHERE session_id = $1`, ann.SessionID).Scan(&byteLen); err != nil {
		t.Fatalf("byte_len: %v", err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT coalesce(sum(length(content)), 0) FROM session_raw_chunks WHERE session_id = $1`,
		ann.SessionID).Scan(&chunkSum); err != nil {
		t.Fatalf("chunk sum: %v", err)
	}
	if byteLen != chunkSum || byteLen != int64(len(whole)) {
		t.Fatalf("byte_len=%d, chunk sum=%d, appended=%d must all match", byteLen, chunkSum, len(whole))
	}

	// SessionRawTo reports that same total and streams exactly those bytes.
	var buf bytes.Buffer
	written, truncated, total, err := st.SessionRawTo(ctx, &buf, ann.SessionID, 0)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if total != int64(len(whole)) || written != int64(len(whole)) || truncated || !bytes.Equal(buf.Bytes(), whole) {
		t.Fatalf("raw mismatch: total=%d written=%d truncated=%v content=%q", total, written, truncated, buf.Bytes())
	}

	// A capped read reports the full total but streams only the cap, flagged truncated.
	var capped bytes.Buffer
	w2, tr2, total2, err := st.SessionRawTo(ctx, &capped, ann.SessionID, 5)
	if err != nil {
		t.Fatalf("capped raw: %v", err)
	}
	if total2 != int64(len(whole)) || w2 != 5 || !tr2 || !bytes.Equal(capped.Bytes(), whole[:5]) {
		t.Fatalf("capped mismatch: total=%d written=%d truncated=%v content=%q", total2, w2, tr2, capped.Bytes())
	}

	// A session that never received an upload is ErrNotFound, not a zero-length read.
	missing := seedTranscript(t, st, "ada", 0)
	if _, _, _, err := st.SessionRawTo(ctx, &bytes.Buffer{}, missing, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing raw: want ErrNotFound, got %v", err)
	}
}
