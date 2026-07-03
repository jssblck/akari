package store

import (
	"fmt"

	"github.com/jssblck/akari/internal/quality"
)

// perTurnTokensExpr is the SQL for one assistant turn's estimated reasoning-token count, the
// primitive behind the per-session settle derivation (gatherObservedThinking). A turn's tokens
// are its exact reasoning-token count where the agent reports one (Codex logs it per turn in
// message_turn_usage), else its reasoning-trace bytes over the agent's calibrated
// bytes-per-token factor.
//
// It is kept in this file (rather than inline in signals.go) because a fleet-wide aggregate over
// the same expression is coming back once the turn-unit problem is fixed: today a Claude "turn"
// is one JSONL content-block row, so a thinking block, the reply text, and each tool call are
// separate rows and only the thinking row reads as thinking. A distribution over those rows is
// dominated by tool-call rows and reads mostly "off", so the fleet panel was pulled until the
// unit is corrected (group rows by the API message id). See issue #98.
//
// mtuAlias is the message_turn_usage row LEFT JOINed on the turn (its reasoning_tokens is 0 when
// absent, so the coalesce falls through to the byte estimate); mAlias is the messages row (its
// thinking_bytes is the reasoning-trace weight); agentExpr is the SQL that yields the session's
// agent (usually "s.agent"), used to pick the divisor.
func perTurnTokensExpr(mAlias, mtuAlias, agentExpr string) string {
	return fmt.Sprintf(
		`CASE WHEN coalesce(%[2]s.reasoning_tokens, 0) > 0 THEN %[2]s.reasoning_tokens::float8
		      ELSE %[1]s.thinking_bytes::float8 / (%[3]s) END`,
		mAlias, mtuAlias, agentDivisorExpr(agentExpr))
}

// agentDivisorExpr builds the bytes-per-token divisor as a SQL CASE over the agent, with the
// numeric factors pulled from quality.ThinkingBytesPerToken so the SQL cannot drift from the Go
// mapping. The ELSE arm uses the same default (the Claude factor) that
// quality.ThinkingBytesPerToken returns for an unknown agent, so a divide never yields NULL even
// for an agent the map does not name.
func agentDivisorExpr(agentExpr string) string {
	return fmt.Sprintf(
		`CASE %s WHEN 'claude' THEN %g WHEN 'codex' THEN %g WHEN 'pi' THEN %g ELSE %g END`,
		agentExpr,
		quality.ThinkingBytesPerToken("claude"),
		quality.ThinkingBytesPerToken("codex"),
		quality.ThinkingBytesPerToken("pi"),
		quality.ThinkingBytesPerToken(""),
	)
}
