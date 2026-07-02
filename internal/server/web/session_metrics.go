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
}

// TurnLatencies pairs each answered user prompt with the reply that followed it and returns the
// wall-clock gap, keyed by the assistant message's ordinal. The rule walks the transcript in
// order holding the ordinal of the most recent user message that carried a timestamp; the FIRST
// following assistant message that also carries a timestamp closes that turn (latency = its time
// minus the anchor's), and the anchor is then cleared so a later assistant reply in the same
// stretch is not double-counted. A new user message resets the anchor, so an interleaved
// exchange measures each prompt against its own reply rather than a distant one.
//
// A pairing whose latency is negative (a clock skew, or timestamps out of order relative to the
// ordinal order) is skipped rather than shown as a nonsense stamp; FmtLatency would render it a
// dash anyway, but dropping it keeps the map to real measurements. Messages missing a timestamp
// are transparent: a user message with no timestamp sets no anchor, and an assistant with no
// timestamp cannot close one.
func TurnLatencies(msgs []store.Message) map[int]time.Duration {
	out := make(map[int]time.Duration)
	// anchor holds the pending user prompt's time; nil means no open turn to close.
	var anchor *time.Time
	for _, m := range msgs {
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
			if anchor == nil || m.Timestamp == nil || m.Timestamp.IsZero() {
				continue
			}
			d := m.Timestamp.Sub(*anchor)
			anchor = nil // this reply closes the turn; a later one is not the answer to this prompt
			if d < 0 {
				continue // out-of-order timestamps: not a real latency
			}
			out[m.Ordinal] = d
		}
	}
	return out
}

// ContextSheds finds the turns where context was shed (a compaction or a clear) and returns a
// ShedMark per such turn, keyed by the ordinal of the message whose usage dropped. It walks the
// messages in ordinal order (their natural transcript order) and, for each message that has a
// usage row, compares that turn's context occupancy against the previous message-with-usage's
// through quality.IsContextReset. Messages without a usage row are skipped, so the comparison is
// always between two turns that actually presented a context, never across a gap of untracked
// messages.
//
// The shed is attributed to the turn AFTER the drop (the smaller, post-compaction context),
// which is where the transcript draws the divider: the divider sits above the message that first
// ran on the shed-down context, marking the boundary the compaction crossed.
func ContextSheds(msgs []store.Message, usage map[int]store.TurnUsage) map[int]ShedMark {
	out := make(map[int]ShedMark)
	var prev int64
	havePrev := false
	for _, m := range msgs {
		u, ok := usage[m.Ordinal]
		if !ok {
			continue
		}
		cur := u.ContextTokens
		if havePrev && quality.IsContextReset(prev, cur) {
			out[m.Ordinal] = ShedMark{FromTokens: prev, ToTokens: cur}
		}
		prev = cur
		havePrev = true
	}
	return out
}

// DuplicatePromptOrdinals marks the user messages that repeat an earlier prompt verbatim, keyed
// by ordinal. A prompt is a repeat when its normalized digest already appeared on an earlier
// eligible prompt. Eligibility mirrors the session-level duplicate count (quality's hygiene
// aggregate): only user messages with current-version facts and not PromptShort are considered,
// because a terse prompt ("yes", "go on") legitimately recurs and would otherwise flag every
// short acknowledgement as a duplicate. The FIRST occurrence of a digest is not marked (it is
// the original); every later occurrence is.
//
// A message without current facts (a superseded classifier or a pre-migration row) carries no
// trustworthy digest, so it neither marks nor seeds: it is invisible to this pass, the same way
// the renderer shows no hygiene badge for it.
func DuplicatePromptOrdinals(msgs []store.Message) map[int]bool {
	seen := make(map[int64]bool)
	out := make(map[int]bool)
	for _, m := range msgs {
		if m.Role != "user" || !m.PromptFactsCurrent || m.PromptShort {
			continue
		}
		if seen[m.PromptDigest] {
			out[m.Ordinal] = true
			continue
		}
		seen[m.PromptDigest] = true
	}
	return out
}

// FmtContextStamp renders a turn's context occupancy as the compact "ctx 82k" stamp the
// transcript shows inline; the full per-class split rides the title (see ContextStampTitle).
func FmtContextStamp(u store.TurnUsage) string {
	return "ctx " + FmtTokensCompact(u.ContextTokens)
}

// ContextStampTitle spells out the classes behind a context stamp for its hover title, so the
// compact figure is explained on inspection: "input 1.2k, cache read 78k, cache write 3.1k,
// output 950". Output is named even though it is excluded from the occupancy figure, so a reader
// can see the whole turn's token shape and understand why the ctx figure omits output.
func ContextStampTitle(u store.TurnUsage) string {
	return fmt.Sprintf("input %s, cache read %s, cache write %s, output %s",
		FmtTokensCompact(u.Input), FmtTokensCompact(u.CacheRead),
		FmtTokensCompact(u.CacheWrite), FmtTokensCompact(u.Output))
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
