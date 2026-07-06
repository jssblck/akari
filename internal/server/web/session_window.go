package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// The transcript renders a bounded window of messages rather than the whole session, so a long
// transcript cannot freeze the tab on first paint. The outline beside it still lists every turn;
// a click on a turn above the window fetches the gap and then scrolls (see app.js). These helpers
// carry the window shape between the handlers and the templates.

// TranscriptWindowSize is how many messages the session page renders initially and how many each
// "Show earlier" click prepends. At roughly two messages per turn it covers the plan's "last 50
// turns" cap while keeping the window arithmetic on the message ordinal, the transcript's stable
// key.
const TranscriptWindowSize = 100

// TranscriptWindow is the windowed transcript a session view renders: the visible messages in
// ascending ordinal order, the seed messages just before them (empty at the transcript head; see
// store.TranscriptSeed for the two-row shape) that prime the TranscriptWalker without being
// rendered, and how many messages precede the window (zero means the window reaches the head and
// no "Show earlier" bar renders).
type TranscriptWindow struct {
	Msgs         []store.Message
	Seed         []store.Message
	EarlierCount int
}

// FullTranscript wraps an entire message slice as an unwindowed TranscriptWindow, for views that
// deliberately render everything (the public session page, which has no fragment endpoint to
// page through).
func FullTranscript(msgs []store.Message) TranscriptWindow {
	return TranscriptWindow{Msgs: msgs}
}

// WindowTail cuts the initial transcript window from an already-loaded full message slice: the
// last size messages, seeded like store.TranscriptSeed (the message just before the window, and
// the last usage-bearing message behind it when the two differ, so a context shed on the window
// seam keeps its divider). The session page loads the full slice anyway (the outline lists every
// turn), so the initial window and its seed are re-slices, not second reads; only the fragment
// endpoints ("Show earlier", the live append) hit the windowed store reads.
func WindowTail(msgs []store.Message, size int) TranscriptWindow {
	if size <= 0 || len(msgs) <= size {
		return TranscriptWindow{Msgs: msgs}
	}
	cut := len(msgs) - size
	seed := []store.Message{msgs[cut-1]}
	if msgs[cut-1].Usage == nil {
		for i := cut - 2; i >= 0; i-- {
			if msgs[i].Usage != nil {
				seed = []store.Message{msgs[i], msgs[cut-1]}
				break
			}
		}
	}
	return TranscriptWindow{Msgs: msgs[cut:], Seed: seed, EarlierCount: cut}
}

// EarlierPath is the "Show earlier" fetch target: the session body fragment for the window
// strictly before the given ordinal.
func EarlierPath(bodyPath string, beforeOrdinal int) string {
	return fmt.Sprintf("%s?before=%d", bodyPath, beforeOrdinal)
}

// EarlierCountLabel names what remains above the rendered window on the "Show earlier" bar.
func EarlierCountLabel(n int) string {
	if n == 1 {
		return "1 earlier message"
	}
	return fmt.Sprintf("%d earlier messages", n)
}
