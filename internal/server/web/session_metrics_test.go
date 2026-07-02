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

// TestTurnLatencies pins the prompt-to-reply pairing: the first timestamped assistant after a
// timestamped user closes the turn (keyed by the assistant's ordinal), a later user resets the
// anchor, a missing timestamp is transparent, and an out-of-order (negative) gap is dropped.
func TestTurnLatencies(t *testing.T) {
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
			got := TurnLatencies(tc.msgs)
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

// TestContextSheds pins the shed markers at the reset boundary: a large-to-small drop fires and
// carries both occupancy figures, a shallow dip does not, a drop from a small early context does
// not, and a message without a usage row is skipped so the comparison never crosses a gap.
func TestContextSheds(t *testing.T) {
	msg := func(ord int, role string) store.Message { return store.Message{Ordinal: ord, Role: role} }
	usageOf := func(ctx int64) store.TurnUsage { return store.TurnUsage{Input: ctx, ContextTokens: ctx} }

	cases := []struct {
		name  string
		msgs  []store.Message
		usage map[int]store.TurnUsage
		want  map[int]ShedMark
	}{
		{
			name: "a compaction sheds context and is marked on the post-drop turn",
			msgs: []store.Message{msg(0, "user"), msg(1, "assistant"), msg(2, "user"), msg(3, "assistant")},
			usage: map[int]store.TurnUsage{
				0: usageOf(50000), 1: usageOf(180000), 2: usageOf(12000), 3: usageOf(60000),
			},
			want: map[int]ShedMark{2: {FromTokens: 180000, ToTokens: 12000}},
		},
		{
			name: "a shallow dip is not a shed",
			msgs: []store.Message{msg(0, "user"), msg(1, "assistant")},
			usage: map[int]store.TurnUsage{
				0: usageOf(120000), 1: usageOf(90000), // more than half remains
			},
			want: map[int]ShedMark{},
		},
		{
			name: "a drop from a small early context is not a shed",
			msgs: []store.Message{msg(0, "user"), msg(1, "assistant")},
			usage: map[int]store.TurnUsage{
				0: usageOf(10000), 1: usageOf(2000), // prior turn below the keep floor
			},
			want: map[int]ShedMark{},
		},
		{
			name: "a message with no usage is skipped, comparison stays between measured turns",
			// Ordinal 1 has no usage row, so the reset compares ordinal 2 against ordinal 0's
			// context directly rather than a missing intervening turn.
			msgs: []store.Message{msg(0, "user"), msg(1, "assistant"), msg(2, "user")},
			usage: map[int]store.TurnUsage{
				0: usageOf(160000), 2: usageOf(10000),
			},
			want: map[int]ShedMark{2: {FromTokens: 160000, ToTokens: 10000}},
		},
		{
			name: "a drop to exactly half from an at-floor context counts",
			msgs: []store.Message{msg(0, "user"), msg(1, "assistant")},
			usage: map[int]store.TurnUsage{
				0: usageOf(20000), 1: usageOf(10000),
			},
			want: map[int]ShedMark{1: {FromTokens: 20000, ToTokens: 10000}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ContextSheds(tc.msgs, tc.usage)
			if len(got) != len(tc.want) {
				t.Fatalf("sheds = %v, want %v", got, tc.want)
			}
			for ord, want := range tc.want {
				if got[ord] != want {
					t.Errorf("shed[%d] = %+v, want %+v", ord, got[ord], want)
				}
			}
		})
	}
}

// TestDuplicatePromptOrdinals pins the repeat detection: a verbatim digest repeat past its first
// occurrence is marked, a short prompt is excluded (a terse "yes" legitimately recurs even when
// its digest collides with another short prompt), and a message without current facts neither
// marks nor seeds.
func TestDuplicatePromptOrdinals(t *testing.T) {
	up := func(ord int, digest int64, short, current bool) store.Message {
		return store.Message{Ordinal: ord, Role: "user", PromptDigest: digest, PromptShort: short, PromptFactsCurrent: current}
	}
	cases := []struct {
		name string
		msgs []store.Message
		want map[int]bool
	}{
		{
			name: "the second occurrence of a digest is a repeat, the first is not",
			msgs: []store.Message{
				up(0, 111, false, true),
				up(2, 222, false, true),
				up(4, 111, false, true), // repeat of ordinal 0
			},
			want: map[int]bool{4: true},
		},
		{
			name: "short prompts are excluded even when their digests collide",
			// Two terse acknowledgements share a digest; neither is flagged, because a short prompt
			// is not duplicate-eligible.
			msgs: []store.Message{
				up(0, 999, true, true),
				up(1, 999, true, true),
			},
			want: map[int]bool{},
		},
		{
			name: "a message without current facts neither marks nor seeds",
			// The middle message repeats a digest but is at a superseded classifier version, so it is
			// invisible; the later current-facts message that repeats the ORIGINAL is still the first
			// current occurrence and is not marked.
			msgs: []store.Message{
				up(0, 333, false, false), // stale: does not seed
				up(1, 333, false, true),  // first current occurrence of 333: not a repeat
				up(2, 333, false, true),  // second current occurrence: a repeat
			},
			want: map[int]bool{2: true},
		},
		{
			name: "a non-user message is ignored",
			msgs: []store.Message{
				up(0, 444, false, true),
				{Ordinal: 1, Role: "assistant", PromptDigest: 444, PromptFactsCurrent: true},
				up(2, 444, false, true),
			},
			want: map[int]bool{2: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DuplicatePromptOrdinals(tc.msgs)
			if len(got) != len(tc.want) {
				t.Fatalf("dups = %v, want %v", got, tc.want)
			}
			for ord := range tc.want {
				if !got[ord] {
					t.Errorf("expected ordinal %d marked as a repeat", ord)
				}
			}
		})
	}
}
