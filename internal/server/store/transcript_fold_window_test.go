package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedFoldWindow builds one session with ten messages (ordinals 0..9, even ordinals
// user, odd assistant) through the production rebuild, with per-turn usage on
// ordinals 1 and 9, so the windowed full-fold reads can be checked for their window
// arithmetic, the usage fold riding along, and the two-row seed shape (the message
// before a window is usually a user turn without usage, so the usage-bearing seed
// must be looked up behind it). Returns the session id.
func seedFoldWindow(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sid := seedSession(t, st, uid, pid, "sess-fold-window")

	var msgs []store.MessageDelta
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, store.MessageDelta{Ordinal: i, Role: role, Content: "m", Model: "gpt-5"})
	}
	ord1, ord9 := 1, 9
	cost := 0.25
	delta := store.ProjectionDelta{
		Messages: msgs,
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord1, Model: "gpt-5", Input: 100, Output: 20, CacheRead: 400, CacheWrite: 30, CostUSD: &cost, SourceOffset: 1, SourceIndex: 0},
			{MessageOrdinal: &ord9, Model: "gpt-5", Input: 900, Output: 90, CacheRead: 0, CacheWrite: 0, CostUSD: &cost, SourceOffset: 2, SourceIndex: 0},
		},
	}
	rebuildWith(t, st, sid, delta)
	return sid
}

// ordinals flattens a window to its ordinal sequence for assertion.
func ordinals(msgs []store.Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Ordinal
	}
	return out
}

func wantOrdinals(t *testing.T, label string, got []store.Message, want ...int) {
	t.Helper()
	g := ordinals(got)
	if len(g) != len(want) {
		t.Fatalf("%s: got ordinals %v, want %v", label, g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s: got ordinals %v, want %v", label, g, want)
		}
	}
}

// TestMessagesTailWindows pins the tail read the web transcript pages with: the last
// N rows in ascending order, the walker seed behind the window, and the head and
// empty boundaries.
func TestMessagesTailWindows(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedFoldWindow(t, st)

	// The initial view: last 4 of 10. The message before the window (5, an assistant
	// with no usage row) anchors the boundary, and the last usage-bearing message (1)
	// rides ahead of it so a shed on the seam keeps its divider.
	win, seed, err := st.MessagesTail(ctx, sid, nil, 4)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	wantOrdinals(t, "tail(nil, 4) window", win, 6, 7, 8, 9)
	wantOrdinals(t, "tail(nil, 4) seed", seed, 1, 5)
	if seed[0].Usage == nil {
		t.Fatalf("the usage-bearing seed row must carry its fold, got %+v", seed[0])
	}

	// A window wider than the transcript returns everything with no seed.
	win, seed, err = st.MessagesTail(ctx, sid, nil, 20)
	if err != nil {
		t.Fatalf("wide tail: %v", err)
	}
	wantOrdinals(t, "tail(nil, 20) window", win, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	wantOrdinals(t, "tail(nil, 20) seed", seed)

	// "Show earlier" passes the first rendered ordinal: the boundary ordinal itself is
	// excluded (strictly less), so windows never overlap at the seam.
	before := 6
	win, seed, err = st.MessagesTail(ctx, sid, &before, 3)
	if err != nil {
		t.Fatalf("tail before 6: %v", err)
	}
	wantOrdinals(t, "tail(6, 3) window", win, 3, 4, 5)
	wantOrdinals(t, "tail(6, 3) seed", seed, 1, 2)

	// A window that reaches the head comes back short with no seed.
	before = 3
	win, seed, err = st.MessagesTail(ctx, sid, &before, 5)
	if err != nil {
		t.Fatalf("tail before 3: %v", err)
	}
	wantOrdinals(t, "tail(3, 5) window", win, 0, 1, 2)
	wantOrdinals(t, "tail(3, 5) seed", seed)

	// Nothing precedes the first ordinal.
	before = 0
	win, seed, err = st.MessagesTail(ctx, sid, &before, 5)
	if err != nil {
		t.Fatalf("tail before 0: %v", err)
	}
	if len(win) != 0 || len(seed) != 0 {
		t.Fatalf("tail(0, 5) = %v seed %v, want empty window and seed", ordinals(win), ordinals(seed))
	}

	// The full fold rides the window: ordinal 9's usage arrives on the tail row.
	win, _, err = st.MessagesTail(ctx, sid, nil, 1)
	if err != nil {
		t.Fatalf("tail 1: %v", err)
	}
	if len(win) != 1 || win[0].Usage == nil {
		t.Fatalf("tail(nil, 1) must carry the usage fold, got %+v", win)
	}
	if u := win[0].Usage; u.Input != 900 || u.ContextTokens != 900 || u.CostUSD == nil {
		t.Fatalf("tail row usage = %+v, want the folded turn 9 usage", u)
	}
}

// TestMessagesRangeWindows pins the gap read behind the outline's fetch-then-scroll:
// a half-open [from, to) window plus the seed behind it.
func TestMessagesRangeWindows(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedFoldWindow(t, st)

	win, seed, err := st.MessagesRange(ctx, sid, 4, 7)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	wantOrdinals(t, "range(4, 7) window", win, 4, 5, 6)
	wantOrdinals(t, "range(4, 7) seed", seed, 1, 3)

	// From the head: no seed exists.
	win, seed, err = st.MessagesRange(ctx, sid, 0, 2)
	if err != nil {
		t.Fatalf("range from head: %v", err)
	}
	wantOrdinals(t, "range(0, 2) window", win, 0, 1)
	wantOrdinals(t, "range(0, 2) seed", seed)

	// A window starting right after a usage-bearing turn needs only the one seed row.
	_, seed, err = st.MessagesRange(ctx, sid, 2, 4)
	if err != nil {
		t.Fatalf("range after usage turn: %v", err)
	}
	wantOrdinals(t, "range(2, 4) seed", seed, 1)

	// An inverted or empty range returns nothing rather than erroring.
	win, _, err = st.MessagesRange(ctx, sid, 7, 7)
	if err != nil || len(win) != 0 {
		t.Fatalf("range(7, 7) = %v, %v; want empty", ordinals(win), err)
	}

	// The fold rides the range read too.
	win, _, err = st.MessagesRange(ctx, sid, 1, 2)
	if err != nil || len(win) != 1 || win[0].Usage == nil || win[0].Usage.ContextTokens != 530 {
		t.Fatalf("range(1, 2) must carry turn 1's folded usage, got %+v (err %v)", win, err)
	}
}

// TestMessagesAfterFullWindows pins the live-append read: everything strictly past
// the last rendered ordinal, plus the seed at and behind that boundary so the
// appended rows' latency and shed marks stay continuous with the rows on screen.
func TestMessagesAfterFullWindows(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedFoldWindow(t, st)

	win, seed, err := st.MessagesAfterFull(ctx, sid, 5)
	if err != nil {
		t.Fatalf("after 5: %v", err)
	}
	wantOrdinals(t, "afterFull(5) window", win, 6, 7, 8, 9)
	wantOrdinals(t, "afterFull(5) seed", seed, 1, 5)

	// Caught up: nothing to append; the boundary row itself (usage-bearing) is the
	// whole seed.
	win, seed, err = st.MessagesAfterFull(ctx, sid, 9)
	if err != nil {
		t.Fatalf("after 9: %v", err)
	}
	if len(win) != 0 {
		t.Fatalf("afterFull(9) window = %v, want empty", ordinals(win))
	}
	wantOrdinals(t, "afterFull(9) seed", seed, 9)

	// Before the whole transcript: everything, no seed.
	win, seed, err = st.MessagesAfterFull(ctx, sid, -1)
	if err != nil {
		t.Fatalf("after -1: %v", err)
	}
	wantOrdinals(t, "afterFull(-1) window", win, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	wantOrdinals(t, "afterFull(-1) seed", seed)

	// The appended rows carry the full fold: ordinal 9's usage is present.
	win, _, err = st.MessagesAfterFull(ctx, sid, 8)
	if err != nil || len(win) != 1 || win[0].Usage == nil || win[0].Usage.Input != 900 {
		t.Fatalf("afterFull(8) must carry turn 9's folded usage, got %+v (err %v)", win, err)
	}
}

// TestMessageCountBefore pins the remainder count the "Show earlier" bar names.
func TestMessageCountBefore(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedFoldWindow(t, st)

	for _, tc := range []struct{ ordinal, want int }{{0, 0}, {5, 5}, {10, 10}, {100, 10}} {
		n, err := st.MessageCountBefore(ctx, sid, tc.ordinal)
		if err != nil {
			t.Fatalf("count before %d: %v", tc.ordinal, err)
		}
		if n != tc.want {
			t.Errorf("count before %d = %d, want %d", tc.ordinal, n, tc.want)
		}
	}
}
