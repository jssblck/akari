package web

import (
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// The per-message transcript instruments (turn latency, context sheds, duplicate prompts) derive
// from the transcript rows the page already holds. Latency and sheds are computed while walking the
// message slice with only bounded carried state (see TranscriptWalker); the duplicate flag is
// folded in the message read itself (store.Message.DuplicatePrompt). They live apart from the
// templates so their boundaries (a missing timestamp, an out-of-order turn) can be table-tested
// without rendering HTML.

// ShedMark records that a message's turn shed context relative to the turn before it (a
// compaction or a manual clear): FromTokens is the prior turn's context occupancy, ToTokens is
// this turn's. The transcript draws a divider labelled "context shed: <from> -> <to>" above the
// marked message. Both figures are context occupancy (input + cache read + cache write), the
// same three-class sum quality.IsContextReset judges, so the label and the reset decision never
// disagree.
//
// The shed dividers are derived from Message.Usage, the per-message fold grouped by message_ordinal
// with NULL-ordinal usage rows dropped (see store.messagesFullQuery). This is deliberately a
// different fold from the stored session_signals.context_reset_count, which gatherContextHealth
// derives from the RAW usage_events stream in source order (every row, NULL-ordinal ones included,
// no ordinal collapse). The two agree whenever each ordinal carries exactly one dated usage row,
// the shape a real agent produces; they can diverge (a session shows context_reset_count = 1 yet no
// shed divider, or vice versa) only for a multi-row turn or an unattributed row, which the schema
// permits but the parser does not emit. This divergence is intentional and does not need to
// reconcile: the divider attaches to a rendered message, so it can only mark a drop between two
// turns that each presented a context, while the signal measures the raw turn sequence. It is the
// same divergence the per-turn usage fold carries, documented on store.messagesFullQuery and pinned
// by store's TestMessagesTurnUsageDivergesFromContextFold.
type ShedMark struct {
	FromTokens int64
	ToTokens   int64
	// FromUsage and ToUsage carry the full per-turn usage behind the two occupancy figures, so
	// the shed divider's breakdown card can spell out each side's token classes and cost rather
	// than the label's two bare context totals. FromUsage is the prior measured turn's usage,
	// ToUsage this turn's; both are the same *TurnUsage the message carries on Message.Usage
	// (never nil here: a shed is detected only between two turns that both presented a context).
	FromUsage store.TurnUsage
	ToUsage   store.TurnUsage
}

// MsgMetrics is the render-only overlay one transcript message carries: its reply latency (a zero
// duration means the message opens no answered turn) and a shed marker (nil unless this turn shed
// context relative to the previous measured one). The duplicate-prompt verdict is NOT here: it
// rides on store.Message.DuplicatePrompt, folded in the message read, so the walker holds no state
// for it.
type MsgMetrics struct {
	Latency time.Duration
	Shed    *ShedMark
}

// TranscriptWalker derives the two carried per-message instruments (reply latency and context
// sheds) as the render walks the message slice in order, holding only O(1) state between messages
// rather than a second session-sized structure. The render path already holds the message slice
// the page renders; the walker adds nothing that scales with the session beyond that. Instantiate
// one per transcript render and call Next once per message in order.
//
// The carried state is exactly:
//   - anchor: the pending user prompt's timestamp (nil when no turn is open), for latency.
//   - prevContext / prevUsage / havePrev: the previous measured turn's context occupancy and its
//     full usage, for shed detection against the current turn.
//
// The two sub-computations keep their own semantics:
//
//   - Latency pairs each answered user prompt with the reply that followed it. The walk holds the
//     time of the most recent timestamped user message; the FIRST following timestamped assistant
//     message closes that turn (latency = its time minus the anchor's), and the anchor is then
//     cleared so a later reply in the same stretch is not double-counted. A new user message resets
//     the anchor, so an interleaved exchange measures each prompt against its own reply. A negative
//     gap (clock skew, or timestamps out of order relative to ordinal order) is dropped rather than
//     shown as a nonsense stamp. A message missing a timestamp is transparent: a user prompt with
//     none sets no anchor, and an assistant with none cannot close one.
//
//   - Shed marks the turns where context dropped (a compaction or a clear). For each message that
//     carries usage (Message.Usage != nil), it compares that turn's context occupancy against the
//     previous message-with-usage's through quality.IsContextReset; messages without usage are
//     skipped, so the comparison is always between two turns that actually presented a context. The
//     shed is attributed to the turn AFTER the drop (the smaller, post-compaction context), where
//     the transcript draws the divider: above the message that first ran on the shed-down context.
//     This reads the ordinal-grouped Message.Usage fold, so it can intentionally diverge from
//     session_signals.context_reset_count (the raw-stream fold); see ShedMark for why the two need
//     not reconcile.
//
// The duplicate-prompt badge is not derived here: it is folded in the message read as
// store.Message.DuplicatePrompt over the whole session (see store.messagesFullQuery), so the walker
// needs no digest set and the render reads m.DuplicatePrompt directly.
type TranscriptWalker struct {
	anchor      *time.Time
	prevContext int64
	prevUsage   store.TurnUsage
	havePrev    bool
}

// SeededWalker returns a walker primed with the unrendered rows that precede a
// transcript window, so the window's first rendered turns still carry their latency and
// shed stamps. The seed is the store's fixed short lookback (transcriptSeedLookback);
// a boundary turn whose prompt sits deeper than the lookback simply shows no latency,
// which the P-2 plan accepts. A whole-transcript render passes nil.
func SeededWalker(seed []store.Message) *TranscriptWalker {
	w := &TranscriptWalker{}
	for _, m := range seed {
		w.Next(m)
	}
	return w
}

// Next advances the walker over one message and returns that message's latency and shed marks.
// Messages must be passed in transcript (ordinal) order, once each; the walker's carried state
// depends on the order.
func (w *TranscriptWalker) Next(m store.Message) MsgMetrics {
	var out MsgMetrics

	// Latency: reopen or close a turn on the role.
	switch m.Role {
	case "user":
		// A new prompt (re)opens the turn, whether or not the prior one was answered, so a reply is
		// always measured against the most recent prompt that preceded it.
		if m.Timestamp != nil && !m.Timestamp.IsZero() {
			t := *m.Timestamp
			w.anchor = &t
		} else {
			w.anchor = nil
		}
	case "assistant":
		if w.anchor != nil && m.Timestamp != nil && !m.Timestamp.IsZero() {
			d := m.Timestamp.Sub(*w.anchor)
			w.anchor = nil // this reply closes the turn; a later one is not its answer
			if d >= 0 {    // an out-of-order (negative) gap is not a real latency
				out.Latency = d
			}
		}
	}

	// Shed: compare this turn's context occupancy against the previous measured turn's.
	if m.Usage != nil {
		u := *m.Usage
		if w.havePrev && quality.IsContextReset(w.prevContext, u.ContextTokens) {
			out.Shed = &ShedMark{
				FromTokens: w.prevContext,
				ToTokens:   u.ContextTokens,
				FromUsage:  w.prevUsage,
				ToUsage:    u,
			}
		}
		w.prevContext = u.ContextTokens
		w.prevUsage = u
		w.havePrev = true
	}

	return out
}

// FmtContextStamp renders a turn's context occupancy as the compact "ctx 82k" stamp the
// transcript shows inline; the full per-class split and cost ride the breakdown card the stamp
// hosts (see turnCard in session.templ), not a hover title.
func FmtContextStamp(u store.TurnUsage) string {
	return "ctx " + FmtTokensCompact(u.ContextTokens)
}

// TurnTokenTotal is the four-class spend total for one turn (input + output + cache read + cache
// write), the headline the turn's breakdown card carries above its per-class split. It is a real
// all-four-classes total, distinct from the context-occupancy figure the visible "ctx" stamp
// shows (which excludes output), so the card reconciles with the four rows beneath it.
func TurnTokenTotal(u store.TurnUsage) int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// TurnCostLabel renders a turn's cost for its breakdown card: the dollar figure when the turn was
// priced, or "unpriced" when it carries no cost (a turn whose usage never got a rate), so the card
// never shows a misleading "$0.00" for an unmeasured cost. A priced-but-incomplete turn (some of
// its token-bearing rows were unpriced) renders the same "$X+" lower-bound marker every other
// token/cost figure uses, so the card's cost never reads as exact next to unpriced token rows.
func TurnCostLabel(u store.TurnUsage) string {
	if u.CostUSD == nil {
		return "unpriced"
	}
	return FmtCost(*u.CostUSD, u.CostIncomplete)
}

// ShedLabel renders a shed divider's centered label, "context shed: 145k -> 12k", with a real
// arrow between the before and after occupancy figures.
func ShedLabel(m ShedMark) string {
	return fmt.Sprintf("context shed: %s → %s", FmtTokensCompact(m.FromTokens), FmtTokensCompact(m.ToTokens))
}

// FmtTurnLatency renders a turn's reply latency as the inline "+6s" stamp: the leading plus
// reads it as an elapsed gap from the prompt rather than an absolute time. It reuses FmtLatency
// for the unit split.
func FmtTurnLatency(d time.Duration) string {
	return "+" + FmtLatency(d)
}

// signalsFor rebuilds the pure quality.Signals from a session's stored SessionSignals, so the
// quality tooltip can call quality.ScoreBreakdown (and reuse the exact scoring arithmetic)
// rather than re-deriving penalties in the template. The fields align one-to-one; the outcome
// is the one that needs a cast from the stored string back to quality.Outcome (the stored value
// is one of the same four constants).
func signalsFor(s store.SessionSignals) quality.Signals {
	return quality.Signals{
		ToolCalls:            s.ToolCalls,
		ToolFailures:         s.ToolFailures,
		ToolRetries:          s.ToolRetries,
		EditChurn:            s.EditChurn,
		LongestFailureStreak: s.LongestFailureStreak,
		Outcome:              quality.Outcome(s.Outcome),
	}
}

// ScoreBreakdownItems returns the penalty lines behind a scored session's grade (label plus the
// points it subtracted), for the "score arithmetic" group in the quality tooltip. It is empty
// when the session is unscored (there is no arithmetic) OR scored with no penalties (a clean
// 100): the caller distinguishes the two through the session's Scored() flag and shows a single
// "no penalties" row for the clean-scored case.
func ScoreBreakdownItems(s store.SessionSignals) []quality.ScoreBreakdownItem {
	return quality.ScoreBreakdown(signalsFor(s))
}

// ToolFilePath is the path a tool chip and outline step should DISPLAY: the worktree-relative
// form when it is known, else the absolute path. The same repo file then reads the same across
// the worktrees it was edited from.
func ToolFilePath(t store.ToolCallView) string {
	if t.FileRelPath != "" {
		return t.FileRelPath
	}
	return t.FilePath
}

// ToolFileTitle is the hover title for a tool chip's path: the absolute path when it differs
// from the displayed (relative) form, so the full location is one hover away without cluttering
// the chip. It is empty when the two are the same (a path with no relative form), so no
// redundant title is attached.
func ToolFileTitle(t store.ToolCallView) string {
	if t.FileRelPath != "" && t.FilePath != "" && t.FileRelPath != t.FilePath {
		return t.FilePath
	}
	return ""
}
