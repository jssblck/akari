package store

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/quality"
)

// perTurnTokensExpr is the SQL for one assistant turn's estimated reasoning-token count,
// the shared primitive behind both the per-session settle derivation (gatherObservedThinking)
// and the fleet turn distribution here. A turn's tokens are its exact reasoning-token count
// where the agent reports one (Codex logs it per turn in message_turn_usage), else its
// reasoning-trace bytes over the agent's calibrated bytes-per-token factor. Keeping the
// expression in one place is what lets the stored session scalars and the read-time fleet
// counts sit on the same token scale.
//
// mtuAlias is the message_turn_usage row LEFT JOINed on the turn (its reasoning_tokens is 0
// when absent, so the coalesce falls through to the byte estimate); mAlias is the messages
// row (its thinking_bytes is the reasoning-trace weight); agentExpr is the SQL that yields
// the session's agent (usually "s.agent"), used to pick the divisor.
func perTurnTokensExpr(mAlias, mtuAlias, agentExpr string) string {
	return fmt.Sprintf(
		`CASE WHEN coalesce(%[2]s.reasoning_tokens, 0) > 0 THEN %[2]s.reasoning_tokens::float8
		      ELSE %[1]s.thinking_bytes::float8 / (%[3]s) END`,
		mAlias, mtuAlias, agentDivisorExpr(agentExpr))
}

// agentDivisorExpr builds the bytes-per-token divisor as a SQL CASE over the agent, with the
// numeric factors pulled from quality.ThinkingBytesPerToken so the SQL cannot drift from the
// Go mapping. The ELSE arm uses the same default (the Claude factor) that
// quality.ThinkingBytesPerToken returns for an unknown agent, so a divide never yields NULL
// even for an agent the map does not name.
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

// ObservedThinkingStats is the cohort's deliberation picture over a scope, read per turn
// rather than per session: how the scoped assistant turns split across off (no reasoning
// block) and the four absolute token bands (low/medium/high/xhigh, the edges in
// quality.ThinkingBucketForTokens), plus the per-turn token percentiles. The per-turn unit
// is the canonical one: a session-level average would collapse to the floor (most turns
// barely reason), so the fleet view reports the turn distribution and its tail directly.
//
// The bands are an absolute cut on the estimated-token scale, not a per-model quartile, so
// the distribution is informative where a quartile split is not: a fleet of light thinkers
// reads mostly off/low, and a shift toward harder work moves the bars, while quartiles always
// read 25% each by construction. The
// cohort is the turns of scoped sessions carrying a current-version, non-stale signals row
// with a thinking measurement (assistant_turns present), the same measured-session set the
// per-session readout and the other insight panels use.
type ObservedThinkingStats struct {
	Turns  int // scoped assistant turns of measured sessions, the denominator
	Off    int // turns that carried no reasoning block
	Low    int
	Medium int
	High   int
	XHigh  int
	// Percentiles of per-turn estimated reasoning tokens over the thinking turns (off turns
	// excluded, so the tail describes the turns that actually reasoned). P99 exposes the heavy
	// tail an average hides; P50 is the typical thinking turn.
	P50 float64
	P90 float64
	P99 float64
}

// HasData reports whether the scope carried any measured turn, so the panel can show a note
// rather than a row of zero bars.
func (t ObservedThinkingStats) HasData() bool { return t.Turns > 0 }

// ObservedThinking aggregates the scoped turn distribution on its own pooled connection.
// Insights threads its snapshot transaction through observedThinkingFrom instead, so the
// cohort shares one MVCC snapshot with the other panels.
func (s *Store) ObservedThinking(ctx context.Context, f AnalyticsFilter) (ObservedThinkingStats, error) {
	return s.observedThinkingFrom(ctx, s.Pool, f)
}

// observedThinkingFrom aggregates the scoped assistant turns into the band distribution and
// the token percentiles from one querier. It walks the messages of measured sessions (a
// current-version, non-stale signals row with a thinking measurement) rather than the stored
// session scalars, so the fleet view is a true per-turn distribution and not an average of
// per-session tails. Each turn's tokens come from perTurnTokensExpr (exact where the agent
// reports them, else the byte estimate); the band edges are bound as parameters so the SQL
// filters and quality.ThinkingBucketForTokens cannot drift.
func (s *Store) observedThinkingFrom(ctx context.Context, q querier, f AnalyticsFilter) (ObservedThinkingStats, error) {
	filter, args := f.clauseFor("s.started_at")
	args = append(args, quality.Version)
	version := len(args)
	args = append(args, quality.ThinkingLowMaxTokens, quality.ThinkingMediumMaxTokens, quality.ThinkingHighMaxTokens)
	lowEdge := fmt.Sprintf("$%d", version+1)
	medEdge := fmt.Sprintf("$%d", version+2)
	highEdge := fmt.Sprintf("$%d", version+3)
	var t ObservedThinkingStats
	err := q.QueryRow(ctx,
		`WITH turns AS (
		   SELECT m.has_thinking,
		          CASE WHEN m.has_thinking THEN `+perTurnTokensExpr("m", "mtu", "s.agent")+` END AS tok
		     FROM messages m
		     JOIN sessions s ON s.id = m.session_id
		     JOIN session_signals sig
		       ON sig.session_id = s.id
		      AND sig.signals_version = $`+fmt.Sprint(version)+`
		      AND sig.assistant_turns IS NOT NULL
		     LEFT JOIN message_turn_usage mtu
		       ON mtu.session_id = m.session_id AND mtu.message_ordinal = m.ordinal
		    WHERE m.role = 'assistant'
		      AND NOT s.signals_stale`+filter+`
		 )
		 SELECT count(*),
		        count(*) FILTER (WHERE NOT has_thinking),
		        count(*) FILTER (WHERE has_thinking AND tok <= `+lowEdge+`),
		        count(*) FILTER (WHERE has_thinking AND tok > `+lowEdge+` AND tok <= `+medEdge+`),
		        count(*) FILTER (WHERE has_thinking AND tok > `+medEdge+` AND tok <= `+highEdge+`),
		        count(*) FILTER (WHERE has_thinking AND tok > `+highEdge+`),
		        coalesce(percentile_cont(0.5)  WITHIN GROUP (ORDER BY tok) FILTER (WHERE has_thinking), 0),
		        coalesce(percentile_cont(0.9)  WITHIN GROUP (ORDER BY tok) FILTER (WHERE has_thinking), 0),
		        coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY tok) FILTER (WHERE has_thinking), 0)
		   FROM turns`,
		args...).Scan(&t.Turns, &t.Off, &t.Low, &t.Medium, &t.High, &t.XHigh, &t.P50, &t.P90, &t.P99)
	if err != nil {
		return ObservedThinkingStats{}, fmt.Errorf("observed thinking: %w", err)
	}
	return t, nil
}
