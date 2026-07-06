package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// msgsOfLen builds n bare messages with ordinals 0..n-1, enough for window arithmetic.
func msgsOfLen(n int) []store.Message {
	out := make([]store.Message, n)
	for i := range out {
		out[i] = store.Message{Ordinal: i, Role: "user", Content: "m"}
	}
	return out
}

// TestWindowTail pins the initial-window re-slice: the last size messages, the seed
// just above them, and the count of what remains, with the no-window cases (short
// transcript, non-positive size) passing the slice through whole.
func TestWindowTail(t *testing.T) {
	msgs := msgsOfLen(10)

	win := WindowTail(msgs, 4)
	if len(win.Msgs) != 4 || win.Msgs[0].Ordinal != 6 || win.Msgs[3].Ordinal != 9 {
		t.Fatalf("window = %+v, want ordinals 6..9", win.Msgs)
	}
	// No message carries usage, so the seed is just the boundary row.
	if len(win.Seed) != 1 || win.Seed[0].Ordinal != 5 {
		t.Fatalf("seed = %+v, want just ordinal 5", win.Seed)
	}
	if win.EarlierCount != 6 {
		t.Fatalf("earlier = %d, want 6", win.EarlierCount)
	}

	// With usage present behind the boundary, the last usage-bearing message rides
	// ahead of the boundary row (mirroring store.TranscriptSeed), so a shed on the
	// window seam keeps its divider.
	withUsage := msgsOfLen(10)
	withUsage[3].Usage = &store.TurnUsage{ContextTokens: 120000}
	win = WindowTail(withUsage, 4)
	if len(win.Seed) != 2 || win.Seed[0].Ordinal != 3 || win.Seed[1].Ordinal != 5 {
		t.Fatalf("seed = %+v, want ordinals [3 5] (usage row, then boundary row)", win.Seed)
	}

	// A boundary row that itself carries usage needs no lookback.
	withUsage[5].Usage = &store.TurnUsage{ContextTokens: 110000}
	win = WindowTail(withUsage, 4)
	if len(win.Seed) != 1 || win.Seed[0].Ordinal != 5 {
		t.Fatalf("seed = %+v, want just the usage-bearing boundary row 5", win.Seed)
	}

	for _, size := range []int{10, 20, 0, -1} {
		win = WindowTail(msgs, size)
		if len(win.Msgs) != 10 || win.Seed != nil || win.EarlierCount != 0 {
			t.Fatalf("WindowTail(size=%d) must pass the slice through whole, got %d msgs, seed %v, earlier %d",
				size, len(win.Msgs), win.Seed, win.EarlierCount)
		}
	}

	if w := FullTranscript(msgs); len(w.Msgs) != 10 || w.Seed != nil || w.EarlierCount != 0 {
		t.Fatalf("FullTranscript must wrap the slice unwindowed, got %+v", w)
	}
}

// TestWalkerSeed pins that priming the walker with the messages before a window
// carries the two boundary instruments across the cut: a user seed anchors the
// first row's reply latency, a usage-bearing seed arms shed detection (riding ahead
// of a usage-less boundary row when needed), and an empty seed leaves the walker
// cold.
func TestWalkerSeed(t *testing.T) {
	base := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)

	// Latency across the boundary: the window starts at the assistant reply; the seed
	// is the prompt that opened the turn.
	w := &TranscriptWalker{}
	w.Seed([]store.Message{{Ordinal: 4, Role: "user", Timestamp: tsAt(base, 0)}})
	got := w.Next(store.Message{Ordinal: 5, Role: "assistant", Timestamp: tsAt(base, 7)})
	if got.Latency != 7*time.Second {
		t.Errorf("seeded latency = %v, want 7s", got.Latency)
	}

	// Shed across the boundary, two-row seed: the last usage-bearing turn held a large
	// context, the boundary row is a usage-less user prompt, and the window's first
	// turn presents a sharply smaller context. The usage seed must arm the shed AND the
	// user seed must anchor the latency, in one pass.
	w = &TranscriptWalker{}
	w.Seed([]store.Message{
		{Ordinal: 3, Role: "assistant", Usage: &store.TurnUsage{ContextTokens: 150000}},
		{Ordinal: 4, Role: "user", Timestamp: tsAt(base, 0)},
	})
	got = w.Next(store.Message{Ordinal: 5, Role: "assistant", Timestamp: tsAt(base, 6), Usage: &store.TurnUsage{ContextTokens: 9000}})
	if got.Shed == nil || got.Shed.FromTokens != 150000 || got.Shed.ToTokens != 9000 {
		t.Errorf("seeded shed = %+v, want 150000 -> 9000", got.Shed)
	}
	if got.Latency != 6*time.Second {
		t.Errorf("seeded latency = %v, want 6s alongside the shed", got.Latency)
	}

	// An empty seed is a no-op: the first row opens cold, with no latency or shed.
	w = &TranscriptWalker{}
	w.Seed(nil)
	got = w.Next(store.Message{Ordinal: 0, Role: "assistant", Timestamp: tsAt(base, 3), Usage: &store.TurnUsage{ContextTokens: 9000}})
	if got.Latency != 0 || got.Shed != nil {
		t.Errorf("cold walker produced metrics %+v, want none", got)
	}
}

// TestTranscriptWindowRendering pins the windowed Transcript markup: the
// "Show earlier" bar renders above the rows exactly when messages remain, keyed to
// the first rendered ordinal, and an unwindowed transcript carries no bar.
func TestTranscriptWindowRendering(t *testing.T) {
	msgs := msgsOfLen(10)

	win := WindowTail(msgs, 4)
	html := renderComponent(t, Transcript(win, "claude", nil, nil, nil, "/api/v1/session/1", "/sessions/1/body"))
	if !strings.Contains(html, `id="transcript-earlier"`) {
		t.Fatalf("windowed transcript missing the show-earlier bar:\n%s", html)
	}
	if !strings.Contains(html, "/sessions/1/body?before=6") {
		t.Errorf("bar must fetch the window before the first rendered ordinal (6):\n%s", html)
	}
	if !strings.Contains(html, "6 earlier messages") {
		t.Errorf("bar must name the remainder:\n%s", html)
	}
	if strings.Contains(html, `id="msg-5"`) || !strings.Contains(html, `id="msg-6"`) {
		t.Errorf("window must render exactly ordinals 6..9:\n%s", html)
	}

	full := renderComponent(t, Transcript(FullTranscript(msgs), "claude", nil, nil, nil, "/api/v1/session/1", "/sessions/1/body"))
	if strings.Contains(full, `id="transcript-earlier"`) {
		t.Errorf("full transcript must carry no show-earlier bar:\n%s", full)
	}

	if singular := EarlierCountLabel(1); singular != "1 earlier message" {
		t.Errorf("EarlierCountLabel(1) = %q", singular)
	}
}
