package web

import (
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// tsAt is a small helper: a *time.Time some seconds past a fixed base, so the latency cases read
// as offsets rather than absolute stamps.
func tsAt(base time.Time, secs int) *time.Time {
	t := base.Add(time.Duration(secs) * time.Second)
	return &t
}

// walk runs a fresh TranscriptWalker over the messages in order and returns each message's metrics
// keyed by ordinal, the shape the render reads as it iterates.
func walk(msgs []store.Message) map[int]MsgMetrics {
	w := &TranscriptWalker{}
	out := map[int]MsgMetrics{}
	for _, m := range msgs {
		out[m.Ordinal] = w.Next(m)
	}
	return out
}

// TestWalkerLatency pins the prompt-to-reply pairing the walker holds in its anchor state: the
// first timestamped assistant after a timestamped user closes the turn (keyed by the assistant's
// ordinal), a later user resets the anchor, a missing timestamp is transparent, and an
// out-of-order (negative) gap is dropped.
func TestWalkerLatency(t *testing.T) {
	base := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		msgs []store.Message
		want map[int]time.Duration
	}{
		{
			name: "one prompt, one reply",
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 0)},
				{Ordinal: 1, Role: "assistant", Timestamp: tsAt(base, 6)},
			},
			want: map[int]time.Duration{1: 6 * time.Second},
		},
		{
			name: "only the first reply closes a turn",
			// Two assistant messages follow one prompt; only the first is the answer, the second
			// (with no intervening user) is not measured.
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 0)},
				{Ordinal: 1, Role: "assistant", Timestamp: tsAt(base, 4)},
				{Ordinal: 2, Role: "assistant", Timestamp: tsAt(base, 9)},
			},
			want: map[int]time.Duration{1: 4 * time.Second},
		},
		{
			name: "a later user resets the anchor",
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 0)},
				{Ordinal: 1, Role: "user", Timestamp: tsAt(base, 10)}, // supersedes the first prompt
				{Ordinal: 2, Role: "assistant", Timestamp: tsAt(base, 15)},
			},
			want: map[int]time.Duration{2: 5 * time.Second}, // measured against the 10s prompt, not the 0s one
		},
		{
			name: "a prompt with no timestamp sets no anchor",
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: nil},
				{Ordinal: 1, Role: "assistant", Timestamp: tsAt(base, 6)},
			},
			want: map[int]time.Duration{},
		},
		{
			name: "an assistant with no timestamp cannot close a turn",
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 0)},
				{Ordinal: 1, Role: "assistant", Timestamp: nil},
			},
			want: map[int]time.Duration{},
		},
		{
			name: "an out-of-order reply is dropped",
			// The assistant stamp precedes the prompt (clock skew), so the gap is negative and no
			// nonsense latency is recorded.
			msgs: []store.Message{
				{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 10)},
				{Ordinal: 1, Role: "assistant", Timestamp: tsAt(base, 4)},
			},
			want: map[int]time.Duration{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[int]time.Duration{}
			for ord, m := range walk(tc.msgs) {
				if m.Latency > 0 {
					got[ord] = m.Latency
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("latencies = %v, want %v", got, tc.want)
			}
			for ord, want := range tc.want {
				if got[ord] != want {
					t.Errorf("latency[%d] = %v, want %v", ord, got[ord], want)
				}
			}
		})
	}
}

// TestWalkerSheds pins the shed markers at the reset boundary: a large-to-small drop fires and
// carries both occupancy figures (and both turns' full usage for the divider's breakdown card), a
// shallow dip does not, a drop from a small early context does not, and a message without usage is
// skipped so the comparison never crosses a gap. Usage rides on each message's Usage field (nil
// when the ordinal carried no usage), the shape the message read folds it into.
func TestWalkerSheds(t *testing.T) {
	msg := func(ord int, role string) store.Message { return store.Message{Ordinal: ord, Role: role} }
	// withCtx builds a message whose Usage carries the given context occupancy (as input, so
	// ContextTokens equals it), the minimal fixture the shed comparison reads.
	withCtx := func(ord int, role string, ctx int64) store.Message {
		m := msg(ord, role)
		m.Usage = &store.TurnUsage{Input: ctx, ContextTokens: ctx}
		return m
	}

	cases := []struct {
		name string
		msgs []store.Message
		want map[int]ShedMark
	}{
		{
			name: "a compaction sheds context and is marked on the post-drop turn",
			msgs: []store.Message{
				withCtx(0, "user", 50000), withCtx(1, "assistant", 180000),
				withCtx(2, "user", 12000), withCtx(3, "assistant", 60000),
			},
			want: map[int]ShedMark{2: {FromTokens: 180000, ToTokens: 12000}},
		},
		{
			name: "a shallow dip is not a shed",
			msgs: []store.Message{
				withCtx(0, "user", 120000), withCtx(1, "assistant", 90000), // more than half remains
			},
			want: map[int]ShedMark{},
		},
		{
			name: "a drop from a small early context is not a shed",
			msgs: []store.Message{
				withCtx(0, "user", 10000), withCtx(1, "assistant", 2000), // prior turn below the keep floor
			},
			want: map[int]ShedMark{},
		},
		{
			name: "a message with no usage is skipped, comparison stays between measured turns",
			// Ordinal 1 has no usage, so the reset compares ordinal 2 against ordinal 0's context
			// directly rather than a missing intervening turn.
			msgs: []store.Message{
				withCtx(0, "user", 160000), msg(1, "assistant"), withCtx(2, "user", 10000),
			},
			want: map[int]ShedMark{2: {FromTokens: 160000, ToTokens: 10000}},
		},
		{
			name: "a drop to exactly half from an at-floor context counts",
			msgs: []store.Message{
				withCtx(0, "user", 20000), withCtx(1, "assistant", 10000),
			},
			want: map[int]ShedMark{1: {FromTokens: 20000, ToTokens: 10000}},
		},
		{
			name: "the first turn with usage never sheds (no prior turn to compare)",
			msgs: []store.Message{
				withCtx(0, "user", 200000),
			},
			want: map[int]ShedMark{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[int]ShedMark{}
			for ord, m := range walk(tc.msgs) {
				if m.Shed != nil {
					got[ord] = *m.Shed
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("sheds = %v, want %v", got, tc.want)
			}
			for ord, want := range tc.want {
				// Compare only the two occupancy figures; the full-usage fields ride along for the
				// card and are checked separately below.
				if got[ord].FromTokens != want.FromTokens || got[ord].ToTokens != want.ToTokens {
					t.Errorf("shed[%d] = {%d -> %d}, want {%d -> %d}", ord,
						got[ord].FromTokens, got[ord].ToTokens, want.FromTokens, want.ToTokens)
				}
			}
		})
	}

	// A shed carries both turns' full usage so the divider's breakdown card can spell out each
	// side's token classes, not just the two occupancy totals.
	msgs := []store.Message{
		{Ordinal: 0, Role: "user", Usage: &store.TurnUsage{Input: 150000, CacheRead: 30000, CacheWrite: 0, Output: 2000, ContextTokens: 180000}},
		{Ordinal: 1, Role: "assistant", Usage: &store.TurnUsage{Input: 8000, CacheRead: 4000, CacheWrite: 0, Output: 500, ContextTokens: 12000}},
	}
	got := walk(msgs)
	m1 := got[1]
	if m1.Shed == nil {
		t.Fatal("expected a shed marked on ordinal 1")
	}
	if m1.Shed.FromUsage.Input != 150000 || m1.Shed.ToUsage.Input != 8000 {
		t.Errorf("shed usage not threaded: from.Input=%d to.Input=%d, want 150000 / 8000",
			m1.Shed.FromUsage.Input, m1.Shed.ToUsage.Input)
	}
}

// TestWalkerCombinesLatencyAndShed confirms the one walk carries both concerns at once across
// different ordinals, holding only bounded state: a latency on one turn, a shed on a later one, and
// a turn that opens the exchange but carries neither.
func TestWalkerCombinesLatencyAndShed(t *testing.T) {
	base := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	msgs := []store.Message{
		{Ordinal: 0, Role: "user", Timestamp: tsAt(base, 0), Usage: &store.TurnUsage{Input: 5000, ContextTokens: 5000}},
		{Ordinal: 1, Role: "assistant", Timestamp: tsAt(base, 6), Usage: &store.TurnUsage{Input: 180000, ContextTokens: 180000}}, // latency here
		{Ordinal: 2, Role: "user", Timestamp: tsAt(base, 10)},
		{Ordinal: 3, Role: "assistant", Timestamp: tsAt(base, 20), Usage: &store.TurnUsage{Input: 12000, ContextTokens: 12000}}, // shed here
	}
	got := walk(msgs)

	if got[1].Latency != 6*time.Second {
		t.Errorf("ordinal 1 latency = %v, want 6s", got[1].Latency)
	}
	if got[3].Shed == nil || got[3].Shed.FromTokens != 180000 || got[3].Shed.ToTokens != 12000 {
		t.Errorf("ordinal 3 shed = %+v, want {180000 -> 12000}", got[3].Shed)
	}
	// Ordinal 0 opens a turn but carries neither a latency nor a shed.
	if got[0].Latency != 0 || got[0].Shed != nil {
		t.Errorf("ordinal 0 should carry no marks, got %+v", got[0])
	}
}
