package web

import (
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// The per-message transcript instruments (turn latency, context sheds, duplicate prompts) are
// pure functions over the transcript rows and the per-turn usage the page already holds, so the
// same data drives the first render and every SSE body refresh with no extra query. They live
// apart from the templates so their boundaries (a missing timestamp, an out-of-order turn, a
// digest collision) can be table-tested without rendering HTML.

// ShedMark records that a message's turn shed context relative to the turn before it (a
// compaction or a manual clear): FromTokens is the prior turn's context occupancy, ToTokens is
// this turn's. The transcript draws a divider labelled "context shed: <from> -> <to>" above the
// marked message. Both figures are context occupancy (input + cache read + cache write), the
// same three-class sum quality.IsContextReset judges, so the label and the reset decision never
// disagree.
type ShedMark struct {
	FromTokens int64
	ToTokens   int64
	// FromUsage and ToUsage carry the full per-turn usage behind the two occupancy figures, so
	// the shed divider's breakdown card can spell out each side's token classes and cost rather
	// than the label's two bare context totals. FromUsage is the prior measured turn's usage,
	// ToUsage this turn's; both are the same TurnUsage the transcript stamps a message with.
	FromUsage store.TurnUsage
	ToUsage   store.TurnUsage
}

// MsgAnnotation is the render-only overlay one transcript message carries: its reply latency (a
// zero duration means the message opens no answered turn), a shed marker (nil unless this turn
// shed context relative to the previous measured one), and whether its prompt repeats an earlier
// one verbatim. A message with none of the three gets no map entry, so the annotations map holds
// only the ordinals that actually carry a mark.
type MsgAnnotation struct {
	Latency         time.Duration
	Shed            *ShedMark
	DuplicatePrompt bool
}

// TranscriptAnnotations derives the three per-message transcript instruments (turn latency,
// context sheds, and duplicate prompts) in a single linear walk over the messages, keyed by
// ordinal, with an entry only for an ordinal that carries at least one of the three. It replaces
// the three separate per-concern passes with one, so the transcript template calls it once and
// reads each message's marks by ordinal as it renders.
//
// The three sub-computations keep their own semantics, folded into the one pass:
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
//     has a usage row, it compares that turn's context occupancy against the previous
//     message-with-usage's through quality.IsContextReset; messages without a usage row are skipped,
//     so the comparison is always between two turns that actually presented a context. The shed is
//     attributed to the turn AFTER the drop (the smaller, post-compaction context), where the
//     transcript draws the divider: above the message that first ran on the shed-down context.
//
//   - DuplicatePrompt marks a user message whose normalized digest already appeared on an earlier
//     eligible prompt. Eligibility mirrors the session-level duplicate count: only user messages
//     with current-version facts and not PromptShort are considered, because a terse prompt ("yes",
//     "go on") legitimately recurs. The FIRST occurrence of a digest is not marked (the original);
//     every later occurrence is. A message without current facts carries no trustworthy digest, so
//     it neither marks nor seeds, the same way the renderer shows it no hygiene badge.
//
// This pass adds one map bounded by the same message slice the caller already holds. The web
// transcript is deliberately unwindowed (the page renders the whole session, and the SSE body swap
// re-renders it), so the message slice, the tool-call map, and the outline already scale with the
// session by that design; this annotation map keeps peak memory at the page's existing order and
// the per-refresh time at the render's existing order. An incremental server-side rollup for these
// render-only marks would add ingest-path state and complexity to save work the page's own render
// of the full transcript dwarfs, so the single pass is the right shape here, not a stopgap.
func TranscriptAnnotations(msgs []store.Message, usage map[int]store.TurnUsage) map[int]MsgAnnotation {
	out := make(map[int]MsgAnnotation)
	// mark fetches (creating if absent) the annotation for an ordinal, so the three concerns can
	// each set their own field on the same entry without clobbering the others.
	mark := func(ord int, f func(*MsgAnnotation)) {
		a := out[ord]
		f(&a)
		out[ord] = a
	}

	// Latency state: anchor holds the pending user prompt's time; nil means no open turn to close.
	var anchor *time.Time
	// Shed state: the previous measured turn's usage and whether we have one yet.
	var prevUsage store.TurnUsage
	havePrev := false
	// Duplicate-prompt state: the set of normalized digests seen on eligible prompts so far.
	seen := make(map[int64]bool)

	for _, m := range msgs {
		// Latency: reopen or close a turn on the role.
		switch m.Role {
		case "user":
			// A new prompt (re)opens the turn, whether or not the prior one was answered, so a
			// reply is always measured against the most recent prompt that preceded it.
			if m.Timestamp != nil && !m.Timestamp.IsZero() {
				t := *m.Timestamp
				anchor = &t
			} else {
				anchor = nil
			}
		case "assistant":
			if anchor != nil && m.Timestamp != nil && !m.Timestamp.IsZero() {
				d := m.Timestamp.Sub(*anchor)
				anchor = nil // this reply closes the turn; a later one is not its answer
				if d >= 0 {  // an out-of-order (negative) gap is not a real latency
					ord := m.Ordinal
					mark(ord, func(a *MsgAnnotation) { a.Latency = d })
				}
			}
		}

		// Shed: compare this turn's context occupancy against the previous measured turn's.
		if u, ok := usage[m.Ordinal]; ok {
			if havePrev && quality.IsContextReset(prevUsage.ContextTokens, u.ContextTokens) {
				shed := &ShedMark{
					FromTokens: prevUsage.ContextTokens,
					ToTokens:   u.ContextTokens,
					FromUsage:  prevUsage,
					ToUsage:    u,
				}
				mark(m.Ordinal, func(a *MsgAnnotation) { a.Shed = shed })
			}
			prevUsage = u
			havePrev = true
		}

		// DuplicatePrompt: flag a repeat of an earlier eligible prompt's digest.
		if m.Role == "user" && m.PromptFactsCurrent && !m.PromptShort {
			if seen[m.PromptDigest] {
				mark(m.Ordinal, func(a *MsgAnnotation) { a.DuplicatePrompt = true })
			} else {
				seen[m.PromptDigest] = true
			}
		}
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
// priced, or "unpriced" when it carries no cost (a turn whose usage never got a rate), so the
// card never shows a misleading "$0.00" for an unmeasured cost.
func TurnCostLabel(u store.TurnUsage) string {
	if u.CostUSD == nil {
		return "unpriced"
	}
	return FmtCost(*u.CostUSD, false)
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
