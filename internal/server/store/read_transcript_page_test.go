package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedTurns inserts msgCount messages in one statement, alternating user (even
// ordinals) and assistant (odd), so a windowed read's turn boundaries land on known
// ordinals. It returns the session id.
func seedTurns(t *testing.T, st *store.Store, username string, msgCount int) int64 {
	t.Helper()
	ctx := context.Background()
	uid := seedUser(t, st, username)
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, message_count)
		 VALUES ($1,$2,'claude','src-turns','box',$3) RETURNING id`, uid, pid, msgCount).Scan(&sid); err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content)
		 SELECT $1, g, CASE WHEN g % 2 = 0 THEN 'user' ELSE 'assistant' END, 'm' || g
		   FROM generate_series(0, $2 - 1) g`, sid, msgCount); err != nil {
		t.Fatalf("messages: %v", err)
	}
	return sid
}

func ordinals(msgs []store.Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Ordinal
	}
	return out
}

// TestTranscriptWindowCarriesToolsAndAttachments pins that a window's tool calls and
// attachments ride on the page itself, read in the same snapshot as its rows and cut to
// exactly the window's ordinal range: a chip outside the window costs nothing and can
// never pair with rows from a different projection.
func TestTranscriptWindowCarriesToolsAndAttachments(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const total = 240
	sid := seedTurns(t, st, "grace", total)

	// One tool call before the window, one inside; one attachment and one fallback on
	// each side too. The tail window of 240 alternating rows starts at ordinal 140. The
	// attachments FK into the CAS, so each needs a blob row (the hash is the 64-char
	// zero-padded ordinal, unique per test database).
	for _, ord := range []int{10, 141} {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, category)
			 VALUES ($1, $2, 0, 'Bash', 'bash')`, sid, ord); err != nil {
			t.Fatalf("tool call at %d: %v", ord, err)
		}
		sha := fmt.Sprintf("%064d", ord)
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO blobs (sha256, lo_oid, byte_len, media_type)
			 VALUES ($1, lo_create(0), 4, 'image/png')`, sha); err != nil {
			t.Fatalf("blob for %d: %v", ord, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO attachments (session_id, message_ordinal, sha256, media_type, byte_len)
			 VALUES ($1, $2, $3, 'image/png', 4)`, sid, ord, sha); err != nil {
			t.Fatalf("attachment at %d: %v", ord, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger, occurred_at, dedup_key)
			 VALUES ($1, $2, 'claude-fable-5', 'claude-opus-4-8', 'refusal', now(), $3)`,
			sid, ord, fmt.Sprintf("fb-%d", ord)); err != nil {
			t.Fatalf("fallback at %d: %v", ord, err)
		}
	}

	page, err := st.TranscriptTail(ctx, sid, nil)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(page.Tools) != 1 || page.Tools[0].MessageOrdinal != 141 {
		t.Fatalf("window tools = %+v, want just the ordinal-141 call", page.Tools)
	}
	if len(page.Attachments) != 1 || page.Attachments[0].MessageOrdinal != 141 {
		t.Fatalf("window attachments = %+v, want just the ordinal-141 image", page.Attachments)
	}
	if len(page.Fallbacks) != 1 || page.Fallbacks[0].MessageOrdinal == nil || *page.Fallbacks[0].MessageOrdinal != 141 {
		t.Fatalf("window fallbacks = %+v, want just the ordinal-141 notice", page.Fallbacks)
	}

	// The append read carries them the same way.
	after, err := st.TranscriptAfter(ctx, sid, 140)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(after.Tools) != 1 || after.Tools[0].MessageOrdinal != 141 {
		t.Fatalf("append tools = %+v, want just the ordinal-141 call", after.Tools)
	}
	if len(after.Attachments) != 1 || after.Attachments[0].MessageOrdinal != 141 {
		t.Fatalf("append attachments = %+v, want just the ordinal-141 image", after.Attachments)
	}
}

// TestTranscriptTailWindowsByTurn pins the tail window's shape: it covers exactly the
// last TranscriptTailTurns user turns, starts on a user message, reports what precedes
// it (count and walker seed), and pages backward by `before` all the way to the start
// with no gap or overlap.
func TestTranscriptTailWindowsByTurn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	// 120 user turns at even ordinals 0..238, each followed by one assistant row.
	const total = 240
	sid := seedTurns(t, st, "grace", total)

	page, err := st.TranscriptTail(ctx, sid, nil)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	// The 50th user turn from the end sits at ordinal 238 - 2*49 = 140.
	wantStart := total - 2*store.TranscriptTailTurns
	if len(page.Msgs) == 0 || page.Msgs[0].Ordinal != wantStart {
		t.Fatalf("window starts at %d, want %d", page.Msgs[0].Ordinal, wantStart)
	}
	if page.Msgs[0].Role != "user" {
		t.Fatalf("window must start on a turn boundary, got role %q", page.Msgs[0].Role)
	}
	if last := page.Msgs[len(page.Msgs)-1].Ordinal; last != total-1 {
		t.Fatalf("window ends at %d, want the transcript end %d", last, total-1)
	}
	if !page.HasEarlier || page.EarlierCount != wantStart {
		t.Fatalf("earlier = (%v, %d), want (true, %d)", page.HasEarlier, page.EarlierCount, wantStart)
	}
	if len(page.Seed) == 0 || page.Seed[len(page.Seed)-1].Ordinal != wantStart-1 {
		t.Fatalf("seed must end immediately before the window, got %v", ordinals(page.Seed))
	}

	// Walk backward to the start: every ordinal exactly once, in order, across pages.
	var got []int
	got = append(got, ordinals(page.Msgs)...)
	for page.HasEarlier {
		before := page.Msgs[0].Ordinal
		page, err = st.TranscriptTail(ctx, sid, &before)
		if err != nil {
			t.Fatalf("tail before %d: %v", before, err)
		}
		if len(page.Msgs) == 0 {
			t.Fatalf("HasEarlier promised rows before %d but the page is empty", before)
		}
		got = append(ordinals(page.Msgs), got...)
	}
	if len(got) != total {
		t.Fatalf("backward walk covered %d ordinals, want %d", len(got), total)
	}
	for i, ord := range got {
		if ord != i {
			t.Fatalf("ordinal at position %d is %d (gap or overlap)", i, ord)
		}
	}
	if page.EarlierCount != 0 || len(page.Seed) != 0 {
		t.Fatalf("the first window should have nothing before it, got count=%d seed=%v",
			page.EarlierCount, ordinals(page.Seed))
	}
}

// TestTranscriptTailWithoutUserTurns pins the fallbacks behind the turn heuristic: a
// transcript with no user rows (nothing to anchor a turn boundary on) still returns a
// bounded window, cut by the message cap from the tail.
func TestTranscriptTailWithoutUserTurns(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1,$2,'claude','src-assistant-only','box') RETURNING id`, uid, pid).Scan(&sid); err != nil {
		t.Fatalf("session: %v", err)
	}
	const total = 700 // past the message cap of 600
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content)
		 SELECT $1, g, 'assistant', 'm' FROM generate_series(0, $2 - 1) g`, sid, total); err != nil {
		t.Fatalf("messages: %v", err)
	}

	page, err := st.TranscriptTail(ctx, sid, nil)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(page.Msgs) != 600 {
		t.Fatalf("cap should bound the window to 600 rows, got %d", len(page.Msgs))
	}
	if page.Msgs[0].Ordinal != 100 || page.Msgs[len(page.Msgs)-1].Ordinal != total-1 {
		t.Fatalf("capped window should keep the tail, got [%d..%d]",
			page.Msgs[0].Ordinal, page.Msgs[len(page.Msgs)-1].Ordinal)
	}
	if !page.HasEarlier || page.EarlierCount != 100 {
		t.Fatalf("earlier = (%v, %d), want (true, 100)", page.HasEarlier, page.EarlierCount)
	}
}

// TestTranscriptAfterAppends pins the live-append read: rows strictly after the cursor,
// a seed that ends AT the cursor (the boundary instruments compare against exactly the
// last row the client holds), an empty result at the live edge, and the More flag when
// the client is too far behind for one fragment.
func TestTranscriptAfterAppends(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const total = 240
	sid := seedTurns(t, st, "grace", total)

	page, err := st.TranscriptAfter(ctx, sid, 199)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(page.Msgs) != 40 || page.Msgs[0].Ordinal != 200 || page.Msgs[len(page.Msgs)-1].Ordinal != 239 {
		t.Fatalf("append window = %v, want [200..239]", ordinals(page.Msgs))
	}
	if len(page.Seed) == 0 || page.Seed[len(page.Seed)-1].Ordinal != 199 {
		t.Fatalf("seed must end at the cursor row, got %v", ordinals(page.Seed))
	}
	if page.More {
		t.Fatal("40 new rows fit one fragment; More should be false")
	}

	// At the live edge there is nothing to append, but the seed still ends at the
	// cursor row: that is how the handler tells a quiet tick from a cursor the
	// projection no longer has (which must force a resync).
	edge, err := st.TranscriptAfter(ctx, sid, total-1)
	if err != nil {
		t.Fatalf("after edge: %v", err)
	}
	if len(edge.Msgs) != 0 {
		t.Fatalf("append past the end should be empty, got %v", ordinals(edge.Msgs))
	}
	if len(edge.Seed) == 0 || edge.Seed[len(edge.Seed)-1].Ordinal != total-1 {
		t.Fatalf("a quiet tick's seed must still end at the cursor row, got %v", ordinals(edge.Seed))
	}

	// A cursor past the projection's end (an epoch rebuild shrank it) yields a seed
	// that ends short of the cursor, the signal the handler resyncs on.
	gone, err := st.TranscriptAfter(ctx, sid, total+50)
	if err != nil {
		t.Fatalf("after vanished cursor: %v", err)
	}
	if len(gone.Msgs) != 0 || len(gone.Seed) == 0 || gone.Seed[len(gone.Seed)-1].Ordinal == total+50 {
		t.Fatalf("vanished cursor should return no rows and a seed short of the cursor, got msgs=%v seed=%v",
			ordinals(gone.Msgs), ordinals(gone.Seed))
	}

	// A cursor more than the cap behind flags More, so the handler re-renders whole
	// instead of appending a fragment with a gap after it.
	far, err := st.TranscriptAfter(ctx, seedTurns(t, st, "anna", 700), 0)
	if err != nil {
		t.Fatalf("after far behind: %v", err)
	}
	if !far.More || len(far.Msgs) != 600 {
		t.Fatalf("cap overflow should set More with a capped window, got more=%v len=%d", far.More, len(far.Msgs))
	}
}

// TestTranscriptWindowCarriesTurnUsage pins that the windowed reads keep the full
// per-turn fold the whole-session read carries: a message inside the window comes back
// with its message_turn_usage rollup on Message.Usage, not the empty columns the MCP
// window read emits.
func TestTranscriptWindowCarriesTurnUsage(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const total = 240
	sid := seedTurns(t, st, "grace", total)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO message_turn_usage (session_id, message_ordinal, input_tokens, output_tokens,
		        cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_sum, cost_count, cost_incomplete)
		 VALUES ($1, 201, 1000, 50, 8000, 200, 0, 0.25, 1, false)`, sid); err != nil {
		t.Fatalf("turn usage: %v", err)
	}

	page, err := st.TranscriptTail(ctx, sid, nil)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	var found bool
	for _, m := range page.Msgs {
		if m.Ordinal != 201 {
			continue
		}
		found = true
		if m.Usage == nil {
			t.Fatal("windowed read dropped the per-turn usage fold")
		}
		if m.Usage.ContextTokens != 1000+8000+200 {
			t.Fatalf("context occupancy = %d, want 9200", m.Usage.ContextTokens)
		}
		if m.Usage.CostUSD == nil || *m.Usage.CostUSD != 0.25 {
			t.Fatalf("turn cost = %v, want 0.25", m.Usage.CostUSD)
		}
	}
	if !found {
		t.Fatal("ordinal 201 should be inside the tail window")
	}
}

// TestOutlineMessagesBounded pins the outline read's bounds: every row comes back in
// order, content is cut to its SQL cap, and thinking text (which can be megabytes over
// a session) is never carried.
func TestOutlineMessagesBounded(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1,$2,'claude','src-outline','box') RETURNING id`, uid, pid).Scan(&sid); err != nil {
		t.Fatalf("session: %v", err)
	}
	long := strings.Repeat("x", 2000)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, thinking_text, has_thinking)
		 VALUES ($1, 0, 'user', $2, '', false), ($1, 1, 'assistant', 'short', $2, true)`, sid, long); err != nil {
		t.Fatalf("messages: %v", err)
	}

	msgs, err := st.OutlineMessages(ctx, sid)
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Ordinal != 0 || msgs[1].Ordinal != 1 {
		t.Fatalf("outline rows = %v", ordinals(msgs))
	}
	if len(msgs[0].Content) != 512 {
		t.Fatalf("outline content should be cut to 512 bytes, got %d", len(msgs[0].Content))
	}
	if msgs[1].ThinkingText != "" {
		t.Fatal("outline read must not carry thinking text")
	}
	if !msgs[1].HasThinking {
		t.Fatal("outline read should keep the has_thinking flag")
	}
}
