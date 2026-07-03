// Package quality turns a session's behavioral signals into an outcome
// classification and a 0-100 quality score with an A-F grade. It is pure: the store
// gathers the raw counts and facts from a session's projection (its messages and tool
// calls) and hands them here, so the scoring model can be reasoned about and unit
// tested without a database. The model is adapted from agentsview's penalty scheme:
// start at 100 and subtract for the things that make a session go badly (errors,
// retries, churn, an errored ending), then map the remainder to a letter.
package quality

import (
	"strconv"
	"strings"
)

// Version is the signals-and-scoring version. Bump it whenever the set of signals
// computed, the penalty weights, or the grade thresholds change, so a stored row
// records which model produced it. A reparse rebuilds every row to the running
// version (see store.RefreshSessionSignals), and the UI gates parsed views while a
// reparse runs, so a reader never mixes versions mid-rebuild.
//
// Version 1 is the initial signal set: tool-health signals (failures, immediate
// retries, edit churn, the longest failure streak) and an outcome classification that
// together drive the score and grade, plus two informational signal sets that do not
// feed the score. Prompt hygiene (terse, duplicate, and no-code-context prompt counts,
// and an unstructured-start flag) describes the human's input, not the agent's work.
// Context health (peak context tokens and an inferred context-reset count; see
// ContextHealth) describes resource load, not whether the session went well; it reads a
// session's own usage turns, since a subagent is a separate session in its own transcript
// file and never mixes into another session's context.
//
// Version 2 sharpens outcome classification so a settled session gets a verdict where v1
// gave up. Two cases that v1 always left unknown now resolve: an automation run (no human
// turn) that reached a substantive assistant last word and has gone idle reads as
// completed, and a session that ends mid-tool once it has gone idle past the abandoned
// threshold reads as errored for automation (the run was killed or crashed) or abandoned
// for a human (someone interrupted or walked away). This lifts the "unknown outcome, and
// therefore unscored" share without inventing a verdict for a still-live session: a
// pending call or an unsettled automation run stays unknown until the settle pass sees it
// idle. The signal set, the penalty weights, and the grade thresholds are unchanged, so
// scoring of a given Signals value is identical to v1; only the outcome fed into it moved.
//
// Version 3 rides the parse.Epoch 7 -> 8 reparse that re-roles Codex injected framing (the AGENTS.md
// instructions and environment_context block) from the user role to the "context" role. The scoring,
// weights, and thresholds are unchanged, but the prompt-hygiene aggregate now reads a different prompt
// set on those sessions: the framing no longer counts as a prompt, and the unstructured-start flag is
// judged against the real opening prompt rather than the AGENTS.md block. That shifts stored hygiene
// counts and grades, so the version bump makes the analytics count only rebuilt rows and the settle
// pass re-stamp settled sessions once the reparse has re-derived their message roles.
//
// Version 4 added the observed-thinking scalars (informational like context health, never fed into
// the score), riding the Epoch 11 -> 12 reparse that fills messages.thinking_bytes, the per-turn
// reasoning-trace weight the reducer records (the reasoning plaintext length where the agent logs it,
// else the encrypted payload length).
//
// Version 5 reworks the observed-thinking session scalars from a per-model quartile rank into an
// absolute estimated-token scale (see thinking.go). The stored figures change from a byte sum plus a
// cohort model to per-turn token summaries: thinking_tail_tokens (the hardest-decile mean, the session's
// headline volume) and thinking_peak_tokens, each an estimated reasoning-token count (Codex's exact
// per-turn count from message_turn_usage where present, else thinking_bytes divided by the agent's
// bytes-per-token factor). No parse.Epoch bump rides this one: both inputs (messages.thinking_bytes and
// message_turn_usage.reasoning_tokens) are already populated by earlier epochs, so the settle pass's
// stale-version reconcile re-derives every row from them with no reparse. Scoring, weights, thresholds,
// and outcome rules are unchanged.
const Version = 5

// PromptFactsVersion stamps the per-message prompt-hygiene facts ClassifyPrompt materializes on
// each human message (see the messages.prompt_* columns and store.gatherPromptHygiene). It is
// deliberately separate from Version: those facts are cached ON the messages row and can only be
// re-derived by re-inserting the message, which happens on a reparse, whereas Version-stamped
// session_signals rows are re-derived by the settle pass from whatever facts the row currently
// holds. Reusing Version would break the settle pass's incremental re-stamp: a Version-only bump
// (a scoring tweak that leaves ClassifyPrompt untouched and ships no reparse) would mark every
// cached fact stale, and the settle pass, which cannot re-derive them, could never grade those
// sessions again.
//
// Bump this ONLY when ClassifyPrompt's output changes (a new or changed prompt flag, a different
// normalization for the duplicate digest), and pair the bump with a parse.Epoch bump so the
// corpus reparses and re-derives every message's facts at the new version. Until that reparse
// reaches a session, gatherPromptHygiene treats its old-version facts as unmeasured and leaves the
// session ungraded, so a stored hygiene count is never computed from a superseded classifier.
const PromptFactsVersion = 1

// Outcome is how a session ended, inferred from its projection. It is a best effort:
// without a terminal marker in the transcript the ending is a heuristic, so every
// outcome carries a Confidence.
type Outcome string

const (
	// OutcomeCompleted: the last substantive turn was the assistant's, with no
	// unresolved tool call and no trailing failures. The agent had the last word.
	OutcomeCompleted Outcome = "completed"
	// OutcomeAbandoned: the last substantive turn was the user's, and the session has
	// been idle long enough that no reply is coming. The human walked away.
	OutcomeAbandoned Outcome = "abandoned"
	// OutcomeErrored: the session ends on a run of failing tool calls, so it stopped in
	// a broken state rather than at a clean answer.
	OutcomeErrored Outcome = "errored"
	// OutcomeUnknown: automated (no human turn), still active, truncated mid-tool, or
	// otherwise not classifiable. Not a verdict, an absence of one.
	OutcomeUnknown Outcome = "unknown"
)

// Confidence grades how trustworthy an Outcome is, so the UI can mute a low-confidence
// guess rather than presenting every heuristic as fact.
type Confidence string

const (
	ConfHigh   Confidence = "high"
	ConfMedium Confidence = "medium"
	ConfLow    Confidence = "low"
)

// Facts are the raw, projection-derived inputs to outcome classification. They are
// gathered in the store (one set of cheap aggregates over a session's messages and
// tool calls) and classified here, so the rule lives in one tested place rather than
// in SQL. "Substantive" means a message that carries conversational content, not an
// empty turn that only delivered a tool result.
type Facts struct {
	UserMessages     int  // messages with role=user (any content)
	LastAssistantOrd int  // ordinal of the last substantive assistant message, -1 if none
	LastUserOrd      int  // ordinal of the last substantive user message, -1 if none
	ToolCallPending  bool // a tool call never got a result (ended mid-tool / truncated)
	TrailingFailures int  // length of the run of failing tool calls at the very end
	IdleLongEnough   bool // the session has been inactive past the abandoned threshold
}

// Classify infers a session's Outcome and the Confidence in it from the facts. The
// order is deliberate: the strong, unambiguous signals (no human, mid-tool, an errored
// tail) win before the softer last-word heuristic, so a session that ended badly is
// never read as "completed" merely because the assistant spoke last.
func Classify(f Facts) (Outcome, Confidence) {
	switch {
	case f.TrailingFailures >= 3:
		// A run of three or more failing tool calls at the tail is objective evidence
		// that the session stopped in a broken state, and it stands independent of who
		// was in the loop, so it wins first. This is safe against a still-live session:
		// a pending call at the very end has a NULL result_status, which breaks the
		// trailing-error suffix the store's gather query counts, so TrailingFailures is 0
		// whenever the last call is still pending. A tail run this long therefore only
		// forms once those calls have actually resolved as failures.
		return OutcomeErrored, ConfHigh
	case f.ToolCallPending && !f.IdleLongEnough:
		// A tool call with no result on a session that is still recent may simply be
		// mid-action (the run is live, or the tool is slow), so hold the verdict rather
		// than call a working session broken.
		return OutcomeUnknown, ConfLow
	case f.ToolCallPending && f.UserMessages == 0:
		// A run that died mid-tool and has since gone idle, with no human ever in the
		// loop, is automation that was killed or crashed: nothing came back to resume it.
		// Errored, at medium confidence because we infer the death from the idle gap
		// rather than a terminal marker.
		return OutcomeErrored, ConfMedium
	case f.ToolCallPending:
		// A human session that stopped mid-tool and has gone idle past the threshold: the
		// person interrupted the tool or walked away before it finished. Abandoned, at
		// medium confidence for the same heuristic-idle reason.
		return OutcomeAbandoned, ConfMedium
	case f.UserMessages == 0:
		// No human turn at all: a subagent or scripted run. If it reached a substantive
		// assistant last word and has settled (idle past the threshold, so no more is
		// coming), it delivered an answer and ran to completion: completed at medium
		// confidence, medium because no human ever confirmed the result. A run that is not
		// yet idle stays unknown so a live streaming subagent is never graded early; the
		// settle pass re-grades it once it settles. No assistant word at all (only tool
		// plumbing) leaves nothing to read.
		if f.LastAssistantOrd >= 0 && f.IdleLongEnough {
			return OutcomeCompleted, ConfMedium
		}
		return OutcomeUnknown, ConfLow
	case f.LastAssistantOrd < 0 && f.LastUserOrd < 0:
		// No substantive turn from either side (only tool plumbing): nothing to read.
		return OutcomeUnknown, ConfLow
	case f.LastAssistantOrd > f.LastUserOrd:
		// The assistant had the last substantive word with no unresolved tool and no
		// errored tail, the clearest "it finished" signal the transcript offers.
		return OutcomeCompleted, ConfHigh
	case f.LastUserOrd > f.LastAssistantOrd:
		// The user spoke last. If the session has gone quiet long enough, the human
		// left without a reply; if it is still recent it may simply be in progress, so
		// hold the verdict rather than calling a live session abandoned.
		if f.IdleLongEnough {
			return OutcomeAbandoned, ConfMedium
		}
		return OutcomeUnknown, ConfLow
	default:
		return OutcomeUnknown, ConfLow
	}
}

// Signals are the scored inputs: the tool-health counts plus the classified outcome.
// The store fills these from a session's projection and passes them to Score.
type Signals struct {
	ToolCalls            int
	ToolFailures         int
	ToolRetries          int // immediate retries: a tool re-invoked with the identical input back to back
	EditChurn            int // repeat edits to one file beyond the first
	LongestFailureStreak int
	Outcome              Outcome
}

// Penalty weights and caps, adapted from agentsview's v0.23 model. Each kind of
// trouble subtracts from a perfect 100, capped so one noisy dimension cannot sink the
// whole score on its own.
const (
	penErrored         = 30
	penAbandoned       = 15
	penPerFailure      = 3
	capFailures        = 30
	penPerRetry        = 5
	capRetries         = 25
	penPerChurn        = 4
	capChurn           = 20
	penFailureStreak   = 10 // a run of 3+ consecutive tool failures anywhere in the session
	failureStreakFloor = 3
)

// hasToolSignal reports whether any negative tool-health signal fired. A session with
// no such signal and an unknown outcome is left unscored: there is nothing to grade
// down and no outcome to grade up, so a letter would overstate what is known.
func (s Signals) hasToolSignal() bool {
	return s.ToolFailures > 0 || s.ToolRetries > 0 || s.EditChurn > 0 || s.LongestFailureStreak > 0
}

// Score returns the 0-100 quality score, its A-F grade, and whether the session is
// scored at all. An unknown-outcome session with no tool signal is unscored
// (scored=false): scoring it would invent a verdict the transcript does not support.
// Otherwise the score starts at 100 and subtracts the capped penalties.
func Score(s Signals) (score int, grade string, scored bool) {
	if s.Outcome == OutcomeUnknown && !s.hasToolSignal() {
		return 0, "", false
	}
	penalty := 0
	switch s.Outcome {
	case OutcomeErrored:
		penalty += penErrored
	case OutcomeAbandoned:
		penalty += penAbandoned
	}
	penalty += min(s.ToolFailures*penPerFailure, capFailures)
	penalty += min(s.ToolRetries*penPerRetry, capRetries)
	penalty += min(s.EditChurn*penPerChurn, capChurn)
	if s.LongestFailureStreak >= failureStreakFloor {
		penalty += penFailureStreak
	}
	score = 100 - penalty
	if score < 0 {
		score = 0
	}
	return score, GradeFor(score), true
}

// ScoreBreakdownItem is one line of the score arithmetic: a human-readable label for a
// penalty and the positive number of points it subtracted. The UI stacks these to show a
// reader why a session scored what it did, so a low grade is explained rather than
// asserted.
type ScoreBreakdownItem struct {
	Label  string // e.g. "errored ending", "3 tool failures"
	Points int    // the penalty subtracted, always > 0
}

// ScoreBreakdown returns the penalty lines behind Score for the same Signals, in the same
// order Score applies them (outcome, failures, retries, churn, streak) and with the same
// caps, so the sum of the returned Points equals 100 minus the score (before the clamp at
// zero). It returns nil for an unscored session (the same gating as Score: an unknown
// outcome with no tool signal), since there is no arithmetic to explain. A scored session
// with no penalties (a clean completed run) also returns nil: there is nothing subtracted,
// so there is nothing to list.
func ScoreBreakdown(s Signals) []ScoreBreakdownItem {
	if s.Outcome == OutcomeUnknown && !s.hasToolSignal() {
		return nil
	}
	var items []ScoreBreakdownItem
	switch s.Outcome {
	case OutcomeErrored:
		items = append(items, ScoreBreakdownItem{Label: "errored ending", Points: penErrored})
	case OutcomeAbandoned:
		items = append(items, ScoreBreakdownItem{Label: "abandoned", Points: penAbandoned})
	}
	if p := min(s.ToolFailures*penPerFailure, capFailures); p > 0 {
		items = append(items, ScoreBreakdownItem{Label: plural(s.ToolFailures, "tool failure"), Points: p})
	}
	if p := min(s.ToolRetries*penPerRetry, capRetries); p > 0 {
		items = append(items, ScoreBreakdownItem{Label: plural(s.ToolRetries, "retry"), Points: p})
	}
	if p := min(s.EditChurn*penPerChurn, capChurn); p > 0 {
		items = append(items, ScoreBreakdownItem{Label: plural(s.EditChurn, "churned edit"), Points: p})
	}
	if s.LongestFailureStreak >= failureStreakFloor {
		items = append(items, ScoreBreakdownItem{Label: "failure streak", Points: penFailureStreak})
	}
	return items
}

// plural renders a count with a noun, pluralized by a trailing "s" (or "ies" when the
// singular ends in "y", so "retry" becomes "2 retries"), so a breakdown label reads
// grammatically for the handful of nouns Score uses.
func plural(n int, singular string) string {
	noun := singular
	if n != 1 {
		if strings.HasSuffix(singular, "y") {
			noun = strings.TrimSuffix(singular, "y") + "ies"
		} else {
			noun = singular + "s"
		}
	}
	return strconv.Itoa(n) + " " + noun
}

// Archetype is a coarse shape-of-session label, the kind agentsview surfaces to let a
// reader see at a glance whether their fleet is mostly quick lookups, standard work, or
// long marathons. It is inferred from cheap session facts (how many human turns, how
// many messages, how long it ran), not from the quality signals, so it answers "what
// kind of session was this" rather than "how did it go".
type Archetype string

const (
	// ArchetypeAutomation: no human turn at all, a subagent or scripted run. Checked
	// first so an automated job never reads as a "quick" human session.
	ArchetypeAutomation Archetype = "automation"
	// ArchetypeQuick: a short exchange, a question or a one-step fix.
	ArchetypeQuick Archetype = "quick"
	// ArchetypeStandard: an ordinary working session.
	ArchetypeStandard Archetype = "standard"
	// ArchetypeDeep: a long, involved session.
	ArchetypeDeep Archetype = "deep"
	// ArchetypeMarathon: an exceptionally long-running or message-heavy session.
	ArchetypeMarathon Archetype = "marathon"
)

// Archetype thresholds. A session lands in the heaviest band whose duration OR message
// count it reaches, so a long-but-quiet session and a short-but-chatty one both read as
// substantial. The store builds the same banding in SQL from these constants (one
// numeric source), and ClassifyArchetype is the tested reference.
const (
	MarathonMinutes  = 120
	MarathonMessages = 200
	DeepMinutes      = 30
	DeepMessages     = 60
	StandardMinutes  = 5
	StandardMessages = 15
)

// ArchetypeFacts are the cheap session facts archetype classification reads: the human
// turn count (zero means automation), the total message count, and the wall-clock
// duration in minutes (0 when the session carries no start/end span).
type ArchetypeFacts struct {
	UserMessages int
	Messages     int
	DurationMin  float64
}

// ClassifyArchetype buckets a session by its shape. Automation wins first (no human
// turn). Otherwise the session takes the heaviest band whose duration or message count
// it reaches, so neither a long idle session nor a short burst is undersold.
func ClassifyArchetype(f ArchetypeFacts) Archetype {
	if f.UserMessages == 0 {
		return ArchetypeAutomation
	}
	switch {
	case f.DurationMin >= MarathonMinutes || f.Messages >= MarathonMessages:
		return ArchetypeMarathon
	case f.DurationMin >= DeepMinutes || f.Messages >= DeepMessages:
		return ArchetypeDeep
	case f.DurationMin >= StandardMinutes || f.Messages >= StandardMessages:
		return ArchetypeStandard
	default:
		return ArchetypeQuick
	}
}

// GradeFor maps a 0-100 score to its letter on the standard banding. It is the one
// place the score-to-letter thresholds live, so a per-session grade and a figure
// derived from an average score (the project card's representative grade) band the same
// way rather than drifting apart.
func GradeFor(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}
