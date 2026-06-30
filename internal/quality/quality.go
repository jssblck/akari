// Package quality turns a session's behavioral signals into an outcome
// classification and a 0-100 quality score with an A-F grade. It is pure: the store
// gathers the raw counts and facts from a session's projection (its messages and tool
// calls) and hands them here, so the scoring model can be reasoned about and unit
// tested without a database. The model is adapted from agentsview's penalty scheme:
// start at 100 and subtract for the things that make a session go badly (errors,
// retries, churn, an errored ending), then map the remainder to a letter.
package quality

// Version is the signals-and-scoring version. Bump it whenever the set of signals
// computed, the penalty weights, or the grade thresholds change, so a stored row
// records which model produced it. A reparse rebuilds every row to the running
// version (see store.RefreshSessionSignals), and the UI gates parsed views while a
// reparse runs, so a reader never mixes versions mid-rebuild.
//
// Version 1: tool-health signals (failures, immediate retries, edit churn, the
// longest failure streak) plus an outcome classification. Later versions add
// prompt-hygiene and context-pressure signals.
const Version = 1

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
	UserMessages        int  // messages with role=user (any content)
	LastAssistantOrd    int  // ordinal of the last substantive assistant message, -1 if none
	LastUserOrd         int  // ordinal of the last substantive user message, -1 if none
	ToolCallPending     bool // a tool call never got a result (ended mid-tool / truncated)
	TrailingFailures    int  // length of the run of failing tool calls at the very end
	IdleLongEnough      bool // the session has been inactive past the abandoned threshold
}

// Classify infers a session's Outcome and the Confidence in it from the facts. The
// order is deliberate: the strong, unambiguous signals (no human, mid-tool, an errored
// tail) win before the softer last-word heuristic, so a session that ended badly is
// never read as "completed" merely because the assistant spoke last.
func Classify(f Facts) (Outcome, Confidence) {
	switch {
	case f.UserMessages == 0:
		// No human turn at all: a subagent or an automated run, not something to grade
		// as completed or abandoned.
		return OutcomeUnknown, ConfLow
	case f.ToolCallPending:
		// A tool call with no result means the transcript stops mid-action (the session
		// is still live, or it was truncated). Either way the ending is not yet known.
		return OutcomeUnknown, ConfLow
	case f.TrailingFailures >= 3:
		// The session ends on three or more consecutive failing tool calls, so it
		// stopped in a broken state rather than at a resolved answer.
		return OutcomeErrored, ConfHigh
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
	return score, gradeFor(score), true
}

// gradeFor maps a 0-100 score to its letter on the standard banding.
func gradeFor(score int) string {
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
