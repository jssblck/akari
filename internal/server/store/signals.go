package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/quality"
)

// abandonedIdleMinutes is how long a session whose last substantive turn was the
// user's must stay quiet before its outcome reads "abandoned" rather than "unknown".
// Below it the session may simply be mid-conversation, so the classifier withholds the
// verdict (see quality.Classify); a historical import, long past this window, settles.
const abandonedIdleMinutes = 30

// signalsCurrent is the predicate that admits only a usable signals row: one whose
// session is not flagged signals_stale (the projection has not moved since the
// grade). It is the single definition of "a current, gradeable signal" that every
// fleet read shares. There is no version gate: every derived representation
// versions on parse.Epoch, and the epoch rebuild re-grades the corpus, so a stored
// row is never at a superseded scoring model for longer than one rebuild. It
// carries no join key: a caller pairs it with its own sig.session_id = s.id in a
// JOIN ON or an EXISTS, or drops it straight into a CASE.
func signalsCurrent() string {
	return "NOT s.signals_stale"
}

// SessionSignals is a session's stored behavioral signals: its outcome, its quality
// score and grade (nil when unscored), and the tool-health counts the score is built
// from. It is the read shape of the session_signals row, derived from the session's
// own projection and materialized by the settle pass once the session settles, or
// re-derived on reparse.
type SessionSignals struct {
	SessionID            int64
	Outcome              string
	OutcomeConfidence    string
	Score                *int    // nil when the session is unscored
	Grade                *string // nil when the session is unscored
	ToolCalls            int
	ToolFailures         int
	ToolRetries          int
	EditChurn            int
	LongestFailureStreak int
	// Prompt-hygiene counts describe the human's input, not the agent's work, so they
	// ride alongside the tool-health counts but never feed the score. PromptCount is the
	// classifier's base (non-empty human prompts), the denominator the counts are read
	// against.
	PromptCount          int
	ShortPromptCount     int
	DuplicatePromptCount int
	NoCodeContextCount   int
	UnstructuredStart    bool
	// Context-health figures describe resource load, not the agent's work, so like the
	// hygiene counts they ride alongside the score without feeding it. Both are nil when
	// the session had no usage to measure, so the UI can tell "unmeasured" apart from a
	// measured zero. PeakContextTokens is the heaviest single-turn context the session
	// reached; ContextResetCount is how many inferred context resets (compactions or
	// clears) it went through.
	PeakContextTokens *int64
	ContextResetCount *int
	// Observed-thinking figures: like context health they describe the work's shape, not
	// its quality, so they never feed the score. All four are measured together from the
	// session's assistant turns and are nil when it had none (nothing to measure, so the
	// UI reads absence rather than "off"). AssistantTurns is the denominator, ThinkingTurns
	// how many carried a reasoning block (zero reads as "off"), ThinkingTailTokens the
	// session's headline volume (the mean of the hardest tenth of its thinking turns, in
	// estimated reasoning tokens), and ThinkingPeakTokens the single hardest turn. The band
	// is an absolute cut on the token scale (see quality.ThinkingBucketForTokens).
	AssistantTurns     *int
	ThinkingTurns      *int
	ThinkingTailTokens *int
	ThinkingPeakTokens *int
}

// Scored reports whether the session carries a score and grade, so the UI can show a
// grade tile or fall back to the outcome alone for an unscored (unknown, no-signal)
// session.
func (s SessionSignals) Scored() bool { return s.Score != nil && s.Grade != nil }

// HasToolActivity reports whether the session ran any tools, so the UI can omit the
// tool-health detail for a pure-conversation session that has none.
func (s SessionSignals) HasToolActivity() bool { return s.ToolCalls > 0 }

// HasHygieneSignal reports whether any prompt-hygiene signal fired, so the UI can omit
// the input readout for a session whose prompts were all clean.
func (s SessionSignals) HasHygieneSignal() bool {
	return s.ShortPromptCount > 0 || s.DuplicatePromptCount > 0 ||
		s.NoCodeContextCount > 0 || s.UnstructuredStart
}

// HasContextHealth reports whether the session had usage to measure, so the UI can show the
// context readout only when there is a real figure rather than a blank stand-in. Peak and
// reset count are populated together, so testing the peak is enough.
func (s SessionSignals) HasContextHealth() bool { return s.PeakContextTokens != nil }

// HasThinkingMeasure reports whether the session had assistant turns to measure thinking
// over. The four thinking fields are populated together, so testing the denominator is
// enough. A measured session with zero ThinkingTurns reads as "off"; an unmeasured one
// shows no thinking readout at all.
func (s SessionSignals) HasThinkingMeasure() bool { return s.AssistantTurns != nil }

// ThinkingBucket is the session's absolute band: off when no turn reasoned, else the band
// its hardest-decile-mean volume reaches on the token scale (quality.ThinkingBucketForTokens).
// It reads as ThinkingOff when unmeasured too, so a caller should gate on HasThinkingMeasure
// first to tell "no read" from "measured off".
func (s SessionSignals) ThinkingBucket() quality.ThinkingBucket {
	if !s.HasThinkingMeasure() || s.ThinkingTurns == nil || *s.ThinkingTurns == 0 {
		return quality.ThinkingOff
	}
	return quality.ThinkingBucketForTokens(float64(*s.ThinkingTailTokens))
}

// ThinkingCoverage is the share of the session's assistant turns that carried a reasoning
// block, in [0, 1]. Zero when unmeasured or when no turn reasoned.
func (s SessionSignals) ThinkingCoverage() float64 {
	if !s.HasThinkingMeasure() || *s.AssistantTurns == 0 {
		return 0
	}
	return float64(*s.ThinkingTurns) / float64(*s.AssistantTurns)
}

// signalFacts are the raw, projection-derived inputs a refresh gathers before scoring:
// the tool-health counts that feed quality.Score and the outcome facts that feed
// quality.Classify. Keeping them in one struct lets refreshSignalsTx read them in two
// queries and hand them to the pure scoring model.
type signalFacts struct {
	toolCalls            int
	toolFailures         int
	toolRetries          int
	editChurn            int
	longestFailureStreak int
	trailingFailures     int
	toolPending          bool

	userMessages     int
	lastAssistantOrd int
	lastUserOrd      int
	idleLongEnough   bool

	// hygiene is aggregated from the per-message hygiene columns quality.ClassifyPrompt
	// materialized when each prompt was written (see gatherPromptHygiene): fixed-size facts
	// the refresh sums without reading a prompt body back.
	hygiene quality.PromptHygiene
	// promptCount is the classifier's base: the count of non-empty human prompts it saw,
	// stored so the cohort aggregate divides the hygiene counts by exactly the set they
	// came from rather than by user_message_count (which can include empty-text turns).
	promptCount int

	// Context-health facts, computed from the session's ordered usage. Both are nil when
	// there was no usage to measure (so the row stores NULL, not a misleading zero);
	// peakContextTokens is the heaviest single-turn context and contextResets is the
	// inferred compaction/clear count.
	peakContextTokens *int64
	contextResets     *int

	// Observed-thinking facts, derived per turn from has_thinking, thinking_bytes, and the
	// exact reasoning-token count in message_turn_usage, reduced to the session's tail and
	// peak volume. All nil when the session has no assistant turns.
	assistantTurns     *int
	thinkingTurns      *int
	thinkingTailTokens *int
	thinkingPeakTokens *int
}

// gatherSignalFacts reads a session's tool-health and outcome facts from its
// projection. The tool query first DEDUPES replayed tool calls: a resumed or compacted
// Claude transcript replays prior turns verbatim, so the same call_uid legitimately
// rides several rows (see projection_calluid_test.go), and counting every visible row
// would inflate failures, retries, and churn. Rows sharing a (call_uid, tool, input,
// result) signature collapse to their first occurrence.
//
// A NULL call_uid is never grouped: each distinct no-id call must count once. The
// discriminator lives in its OWN partition column rather than being folded into the
// call_uid column, so a real call_uid can never collide with a synthetic key. Real ids
// sit in the call_uid column (NULL there for no-id rows); the per-row "ord:idx" key
// sits in a second column that is NULL for real-id rows and unique per no-id row, so
// the two namespaces cannot cross even if a transcript's id happened to look like
// "1:0".
func gatherSignalFacts(ctx context.Context, tx pgx.Tx, sessionID int64) (signalFacts, error) {
	var f signalFacts
	// Tool facts over the deduped, ordered tool calls. "Immediate retries" are a tool
	// re-invoked with the identical non-null input as the row right before it; edit
	// churn is repeat edits to one file beyond the first (total edits minus distinct
	// files); the longest failure streak is the max run of consecutive 'error' results
	// (gaps and islands); trailing failures are the run of errors at the very end (the
	// suffix after the last non-error), the signal that classifies an errored ending.
	err := tx.QueryRow(ctx,
		`WITH ranked AS (
		   SELECT message_ordinal, call_index, tool_name, category, file_path,
		          input_sha256, result_status, call_uid,
		          row_number() OVER (
		            PARTITION BY call_uid,
		                         CASE WHEN call_uid IS NULL
		                              THEN message_ordinal::text || ':' || call_index END,
		                         tool_name, coalesce(input_sha256, ''), coalesce(result_status, '')
		            ORDER BY message_ordinal, call_index
		          ) AS rn
		     FROM tool_calls
		    WHERE session_id = $1
		 ),
		 deduped AS (
		   SELECT * FROM ranked WHERE rn = 1
		 ),
		 ordered AS (
		   SELECT result_status, input_sha256, tool_name,
		          row_number() OVER w AS pos,
		          lag(tool_name) OVER w AS prev_tool,
		          lag(input_sha256) OVER w AS prev_input
		     FROM deduped
		   WINDOW w AS (ORDER BY message_ordinal, call_index)
		 ),
		 streak AS (
		   SELECT result_status,
		          row_number() OVER (ORDER BY message_ordinal, call_index)
		            - row_number() OVER (PARTITION BY (result_status = 'error') ORDER BY message_ordinal, call_index) AS grp
		     FROM deduped
		 )
		 SELECT
		   (SELECT count(*) FROM deduped),
		   (SELECT count(*) FROM deduped WHERE result_status = 'error'),
		   (SELECT count(*) FROM ordered WHERE input_sha256 IS NOT NULL AND input_sha256 = prev_input AND tool_name = prev_tool),
		   -- Edit churn counts repeat edits to one file beyond the first, over edits we can
		   -- attribute to a file. An edit whose path did not parse (file_path NULL) is
		   -- excluded from both terms rather than counted as its own churn: two unknown-path
		   -- edits cannot be known to hit the same file, so attributing churn to them would
		   -- invent thrash. The IS NOT NULL guard keeps numerator and denominator over the
		   -- same attributable set (see TestSignalsEditChurnIgnoresUnknownPath).
		   (SELECT coalesce(count(*) - count(DISTINCT file_path), 0) FROM deduped WHERE category = 'edit' AND file_path IS NOT NULL),
		   (SELECT coalesce(max(c), 0) FROM (SELECT count(*) AS c FROM streak WHERE result_status = 'error' GROUP BY grp) s),
		   (SELECT count(*) FROM ordered WHERE pos > coalesce((SELECT max(pos) FROM ordered WHERE result_status IS DISTINCT FROM 'error'), 0)),
		   (SELECT EXISTS (SELECT 1 FROM deduped WHERE result_status IS NULL OR result_status = ''))`,
		sessionID).Scan(
		&f.toolCalls, &f.toolFailures, &f.toolRetries, &f.editChurn,
		&f.longestFailureStreak, &f.trailingFailures, &f.toolPending)
	if err != nil {
		return signalFacts{}, fmt.Errorf("gather tool signals for session %d: %w", sessionID, err)
	}

	// Outcome facts. user_message_count is the rollup (pure tool-result user entries are
	// not messages, so it already counts only real human turns). The last substantive
	// turns require non-empty content, tested through the stored content_length column
	// (octet_length(content), a generated column) rather than content <> '' so the refresh
	// reads fixed-size metadata and never the prompt body. idle is measured against the
	// session's last activity, OR forced by the terminal flag: a session the client
	// declared finished (`akari sync --finalize`) is treated as idle-long-enough now, so
	// quality.Classify renders a verdict immediately instead of withholding it until the
	// abandoned-idle window elapses. The ToolCallPending -> Unknown path in Classify is
	// deliberately left to its own guard: a truncated transcript has no knowable ending
	// whether or not the host called it terminal.
	err = tx.QueryRow(ctx,
		`SELECT s.user_message_count,
		        coalesce((SELECT max(ordinal) FROM messages WHERE session_id = $1 AND role = 'assistant' AND content_length > 0), -1),
		        coalesce((SELECT max(ordinal) FROM messages WHERE session_id = $1 AND role = 'user' AND content_length > 0), -1),
		        (s.terminal OR (s.ended_at IS NOT NULL AND s.ended_at < now() - make_interval(mins => $2)))
		   FROM sessions s WHERE s.id = $1`,
		sessionID, abandonedIdleMinutes).Scan(
		&f.userMessages, &f.lastAssistantOrd, &f.lastUserOrd, &f.idleLongEnough)
	if err != nil {
		return signalFacts{}, fmt.Errorf("gather outcome facts for session %d: %w", sessionID, err)
	}

	// Prompt-hygiene facts, aggregated from the per-message hygiene columns (non-empty user turns
	// only: an empty turn is tool plumbing, not a prompt; role='user' is the real-human-turn set,
	// since the Claude reducer drops tool-result-only user entries; the min-ordinal prompt is the
	// opener the unstructured-start rule reads).
	hygiene, promptCount, err := gatherPromptHygiene(ctx, tx, sessionID)
	if err != nil {
		return signalFacts{}, err
	}
	f.hygiene = hygiene
	f.promptCount = promptCount

	// Context-health facts. Read from the same projection but from usage_events rather
	// than messages, so they live in their own pass over the session's ordered turns.
	if err := gatherContextHealth(ctx, tx, sessionID, &f); err != nil {
		return signalFacts{}, err
	}

	// Observed-thinking facts, one aggregate over the session's assistant turns.
	if err := gatherObservedThinking(ctx, tx, sessionID, &f); err != nil {
		return signalFacts{}, err
	}
	return f, nil
}

// gatherObservedThinking derives a session's observed-thinking scalars from its assistant
// turns: how many turns reasoned, and the session's tail and peak per-turn volume in
// estimated reasoning tokens. Each turn's tokens are its exact reasoning-token count where
// the agent reports one (Codex, in message_turn_usage) else its trace bytes over the agent's
// calibrated bytes-per-token factor (perTurnTokensExpr). A thinking turn is any assistant
// message with has_thinking set (a reasoning block was present), decoupled from whether its
// text survived redaction.
//
// The headline figure is the tail, not a mean over all turns: most turns barely reason, so an
// all-turn average collapses to the floor, while the mean of the hardest tenth
// (ceil(thinking_turns / 10) turns) reads "how hard it thought when it thought hard" without a
// single outlier turn defining the session the way a bare max would. Peak (the hardest single
// turn) rides alongside it. A session with no assistant turns leaves all four facts nil, so
// the row stores NULL (absent, not "off"); one with assistant turns but none that reasoned
// stores zero tail and peak (a measured "off").
func gatherObservedThinking(ctx context.Context, tx pgx.Tx, sessionID int64, f *signalFacts) error {
	var (
		assistant, thinking int
		tail, peak          float64
	)
	// The counts and the peak are plain aggregates over the session's assistant turns (no
	// sort). The tail is the mean of the hardest decile, so it needs only the top
	// ceil(thinking/10) turns by token count: an ORDER BY tok DESC LIMIT k, which Postgres runs
	// as a bounded top-N heapsort holding k rows, not a full sort materializing every thinking
	// turn. This runs in the settle pass (once per settled session, off the ingest hot path,
	// alongside gatherContextHealth's ordered scan), and the bounded top-N keeps its sort memory
	// to the decile rather than the whole accumulated turn history.
	if err := tx.QueryRow(ctx,
		`WITH t AS (
		   SELECT m.has_thinking,
		          CASE WHEN m.has_thinking THEN `+perTurnTokensExpr("m", "mtu", "s.agent")+` END AS tok
		     FROM messages m
		     JOIN sessions s ON s.id = m.session_id
		     LEFT JOIN message_turn_usage mtu
		       ON mtu.session_id = m.session_id AND mtu.message_ordinal = m.ordinal
		    WHERE m.session_id = $1 AND m.role = 'assistant'
		 ),
		 agg AS (
		   SELECT count(*) AS assistant,
		          count(*) FILTER (WHERE has_thinking) AS thinking,
		          coalesce(max(tok), 0) AS peak
		     FROM t
		 ),
		 tail AS (
		   SELECT coalesce(avg(tok), 0) AS tail
		     FROM (
		       SELECT tok FROM t WHERE tok IS NOT NULL
		        ORDER BY tok DESC
		        LIMIT (SELECT GREATEST(1, ceil(thinking::float8 * 0.1))::bigint FROM agg)
		     ) top
		 )
		 SELECT agg.assistant, agg.thinking, tail.tail, agg.peak
		   FROM agg, tail`,
		sessionID).Scan(&assistant, &thinking, &tail, &peak); err != nil {
		return fmt.Errorf("gather observed thinking for session %d: %w", sessionID, err)
	}
	if assistant == 0 {
		return nil // nothing to measure: leave all four facts nil so the row stores NULL
	}
	// Round to whole tokens. tail <= peak holds before rounding (a mean over the top of a set
	// never exceeds its max) and rounding is monotonic, so the peak >= tail check survives.
	tailTok := int(math.Round(tail))
	peakTok := int(math.Round(peak))
	f.assistantTurns = &assistant
	f.thinkingTurns = &thinking
	f.thinkingTailTokens = &tailTok
	f.thinkingPeakTokens = &peakTok
	return nil
}

// gatherContextHealth reads a session's ordered per-turn context sizes and folds them
// into the facts via a streaming quality.ContextHealthFolder. Context size is the whole prompt presented
// that turn: uncached input plus cached read plus cache creation, so the figure is robust
// to prompt caching (an expired cache moves tokens between those buckets without changing
// their sum) and to an unknown model (it is a raw token count, never divided by a window).
// A session's usage is one coherent context: a subagent runs in its own separate session
// file, so its turns never land here and no per-turn carve-out is needed. Order is the
// transcript's own byte order (source_offset, source_index), which is the turns'
// chronological order in an append-only file; the row id is a final tiebreaker so the
// order is total even for the (schema-permitted but parser-never-emitted) case of a NULL
// or repeated source offset, keeping the reset count deterministic. A session with no
// usage leaves both facts nil, so the row stores NULL rather than a measured-looking zero.
func gatherContextHealth(ctx context.Context, tx pgx.Tx, sessionID int64, f *signalFacts) error {
	rows, err := tx.Query(ctx,
		`SELECT input_tokens + cache_read_tokens + cache_write_tokens
		   FROM usage_events
		  WHERE session_id = $1
		  ORDER BY source_offset, source_index, id`, sessionID)
	if err != nil {
		return fmt.Errorf("gather context health for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	// Fold each turn as it streams, holding no whole-session buffer: the folder keeps only
	// the running peak, the reset count, and the previous turn's size (see
	// quality.ContextHealthFolder), so peak memory does not grow with an arbitrarily long
	// session even though usage turns are unbounded in principle.
	var folder quality.ContextHealthFolder
	for rows.Next() {
		var tokens int64
		if err := rows.Scan(&tokens); err != nil {
			return fmt.Errorf("scan context turn for session %d: %w", sessionID, err)
		}
		folder.Add(tokens)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate context turns for session %d: %w", sessionID, err)
	}
	peak, resets, any := folder.Result()
	if !any {
		return nil // no usage to measure: leave both facts nil so the row stores NULL
	}
	f.peakContextTokens = &peak
	f.contextResets = &resets
	return nil
}

// gatherPromptHygiene computes a session's prompt-hygiene signals and the prompt count from the
// per-message facts stored when each message was written (see quality.ClassifyPrompt and the message
// insert in projection.go). Every signal is a fixed-size aggregate over those columns, so the settle
// pass never reads a prompt body back: peak memory on the refresh path does not track the largest
// prompt a session held, no matter how much pasted code one carried. The set is the session's real
// human turns, tested through the stored content_length column (a generated octet_length(content),
// so "non-empty" is fixed-size metadata, not a body comparison).
//
// It is one aggregate over those turns:
//   - Short and NoCodeContext are the counts of the stored per-prompt flags.
//   - Duplicate is repeats beyond the first among the duplicate-eligible prompts, count(*) minus the
//     distinct digests. Eligibility is "not short": duplicateMinWords equals the terse threshold, so a
//     prompt is either short or duplicate-eligible, never both, and the stored prompt_short flag is the
//     eligibility test with no second column or word re-count needed.
//   - UnstructuredStart is the opening turn's verdict: the min-ordinal prompt was short or a bare
//     greeting. bool_or over just that row reads the opener's stored flags without fetching its body.
//
// The facts are recomputed by every rebuild, so they are always the running
// classifier's output; there is no version to reconcile.
func gatherPromptHygiene(ctx context.Context, tx pgx.Tx, sessionID int64) (quality.PromptHygiene, int, error) {
	var (
		h           quality.PromptHygiene
		promptCount int
	)
	if err := tx.QueryRow(ctx,
		`WITH prompts AS (
		   SELECT ordinal, prompt_short, prompt_no_code, prompt_bare_greeting, prompt_digest
		     FROM messages
		    WHERE session_id = $1 AND role = 'user' AND content_length > 0
		 )
		 SELECT
		   count(*),
		   count(*) FILTER (WHERE prompt_short),
		   count(*) FILTER (WHERE prompt_no_code),
		   count(*) FILTER (WHERE NOT prompt_short)
		     - count(DISTINCT prompt_digest) FILTER (WHERE NOT prompt_short),
		   coalesce(bool_or((prompt_short OR prompt_bare_greeting)
		                    AND ordinal = (SELECT min(ordinal) FROM prompts)), FALSE)
		   FROM prompts`,
		sessionID).Scan(&promptCount, &h.Short, &h.NoCodeContext, &h.Duplicate, &h.UnstructuredStart); err != nil {
		return quality.PromptHygiene{}, 0, fmt.Errorf("gather prompt hygiene for session %d: %w", sessionID, err)
	}
	return h, promptCount, nil
}

// refreshSignalsTx recomputes a session's signals from its projection and UPSERTs the
// session_signals row, inside the caller's transaction. It is driven by the settle
// tick (each due session in its own transaction) and by a rebuild of a settled or
// terminal session (in the rebuild transaction, so the signals commit atomically
// with the projection they summarize; the rows it reads are already written and
// visible in-txn). It is a whole-session recompute, not an incremental fold: the
// signals depend on cross-message order (retry runs, failure streaks, the last
// word), which is also why the ingest path never computes them.
func refreshSignalsTx(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	f, err := gatherSignalFacts(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	outcome, conf := quality.Classify(quality.Facts{
		UserMessages:     f.userMessages,
		LastAssistantOrd: f.lastAssistantOrd,
		LastUserOrd:      f.lastUserOrd,
		ToolCallPending:  f.toolPending,
		TrailingFailures: f.trailingFailures,
		IdleLongEnough:   f.idleLongEnough,
	})
	score, grade, scored := quality.Score(quality.Signals{
		ToolCalls:            f.toolCalls,
		ToolFailures:         f.toolFailures,
		ToolRetries:          f.toolRetries,
		EditChurn:            f.editChurn,
		LongestFailureStreak: f.longestFailureStreak,
		Outcome:              outcome,
	})
	var scoreArg, gradeArg any
	if scored {
		scoreArg, gradeArg = score, grade
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO session_signals
		   (session_id, outcome, outcome_confidence, score, grade,
		    tool_calls, tool_failures, tool_retries, edit_churn, longest_failure_streak,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start,
		    peak_context_tokens, context_reset_count,
		    assistant_turns, thinking_turns, thinking_tail_tokens, thinking_peak_tokens,
		    refreshed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21, now())
		 ON CONFLICT (session_id) DO UPDATE SET
		   outcome = EXCLUDED.outcome,
		   outcome_confidence = EXCLUDED.outcome_confidence,
		   score = EXCLUDED.score,
		   grade = EXCLUDED.grade,
		   tool_calls = EXCLUDED.tool_calls,
		   tool_failures = EXCLUDED.tool_failures,
		   tool_retries = EXCLUDED.tool_retries,
		   edit_churn = EXCLUDED.edit_churn,
		   longest_failure_streak = EXCLUDED.longest_failure_streak,
		   prompt_count = EXCLUDED.prompt_count,
		   short_prompt_count = EXCLUDED.short_prompt_count,
		   duplicate_prompt_count = EXCLUDED.duplicate_prompt_count,
		   no_code_context_count = EXCLUDED.no_code_context_count,
		   unstructured_start = EXCLUDED.unstructured_start,
		   peak_context_tokens = EXCLUDED.peak_context_tokens,
		   context_reset_count = EXCLUDED.context_reset_count,
		   assistant_turns = EXCLUDED.assistant_turns,
		   thinking_turns = EXCLUDED.thinking_turns,
		   thinking_tail_tokens = EXCLUDED.thinking_tail_tokens,
		   thinking_peak_tokens = EXCLUDED.thinking_peak_tokens,
		   refreshed_at = now()`,
		sessionID, string(outcome), string(conf), scoreArg, gradeArg,
		f.toolCalls, f.toolFailures, f.toolRetries, f.editChurn, f.longestFailureStreak,
		f.promptCount, f.hygiene.Short, f.hygiene.Duplicate, f.hygiene.NoCodeContext, f.hygiene.UnstructuredStart,
		f.peakContextTokens, f.contextResets,
		f.assistantTurns, f.thinkingTurns, f.thinkingTailTokens, f.thinkingPeakTokens)
	if err != nil {
		return fmt.Errorf("upsert signals for session %d: %w", sessionID, err)
	}
	// Clear the settle-tick due flag now that the grade matches the projection, but only if
	// the session's outcome is stable, meaning idleLongEnough holds (it has settled past the
	// idle window, or the client declared it terminal). A rebuild runs refreshSignalsTx on
	// whatever it rebuilds, including a still-live session whose outcome is not yet stable
	// (abandoned versus unknown turns on the idle gap), so leaving signals_stale set there
	// keeps the settle tick on the hook to re-grade once the session crosses the idle
	// threshold. A settled or terminal session clears the flag and drops out of the settle
	// index until its next projection change. now() is the transaction clock, so a settle
	// refresh that just read idleLongEnough against it stays consistent with the clear.
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET signals_stale = $2 WHERE id = $1`, sessionID, !f.idleLongEnough); err != nil {
		return fmt.Errorf("clear signals_stale for session %d: %w", sessionID, err)
	}
	return nil
}

// RefreshSessionSignals recomputes one session's signals in its own transaction. It is the
// standalone form the settle tick (RefreshSettledSignals) and the tests use; a rebuild
// calls refreshSignalsTx inside its existing transaction instead, so the signals commit
// with the projection rather than in a second round trip.
//
// It grades only a session whose parse state is settled at the running epoch: the
// attempted epoch (see attemptedEpoch) equals it, and the raw bytes are either fully
// parsed or covered by the recorded deterministic failure. Anything else is skipped,
// leaving signals_stale set, because a grade written now would be cleared as current
// while it is not:
//
//   - Attempted epoch ahead (a newer binary's rebuild OR its recorded failure, seen
//     during a rolling deploy): this binary's scoring code does not match, and grading
//     would clear the flag with nothing left to make the newer binary redo it.
//   - Attempted epoch behind, or bytes neither parsed nor pinned (a rebuild is due,
//     e.g. a finalize racing the parse worker right after the last chunk landed): the
//     pending rebuild supersedes anything graded from the current projection, so
//     grading now could stamp signals from a projection that does not cover the bytes.
//   - Pinned failure at the running epoch: gradeable. The drain will never advance the
//     session, so the settle pass grades the surviving projection under the current
//     scoring, which is the failure model's contract.
//
// The rebuild path needs no such check: it grades the projection it just stamped, at
// its own epoch, inside the same transaction. An unset epoch (0, a store wired without
// SetParserEpoch, as in tests) skips nothing, matching the other epoch gates.
func (s *Store) RefreshSessionSignals(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		if s.parserEpoch != 0 {
			var gradeable bool
			err := tx.QueryRow(ctx,
				`SELECT `+attemptedEpoch("")+` = $2
				        AND (parsed_byte_len = byte_len
				             OR (parse_error <> '' AND parse_error_byte_len = byte_len))
				   FROM session_raw
				  WHERE session_id = $1`, sessionID, s.parserEpoch).Scan(&gradeable)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// No raw bytes were ever uploaded (a bare announce): no rebuild can be
				// pending, so grade whatever projection exists.
			case err != nil:
				return fmt.Errorf("check parse state for session %d: %w", sessionID, err)
			case !gradeable:
				return nil
			}
		}
		return refreshSignalsTx(ctx, tx, sessionID)
	})
}

// RefreshSettledSignals recomputes signals for every settled session marked stale. It is
// the catch-up half of signal grading: a rebuild grades a session that is already settled
// or terminal in its own transaction, so this tick exists for the sessions that settle
// BETWEEN rebuilds (the last rebuild ran while the session was live, so its grade was
// withheld and signals_stale stayed set). It grades them once, after they have been idle
// past the abandoned threshold, off the ingest hot path.
//
// A session is due when signals_stale is set AND it is either settled (ended_at at least
// abandonedIdleMinutes in the past) or terminal (the client declared it finished via
// `akari sync --finalize`, so it is gradeable now without waiting out the idle window). The
// flag is the single-table marker that replaces a cross-table due predicate: a rebuild of a
// live session leaves it set, and refreshSignalsTx clears it only when it grades a
// settled-or-terminal session. So it captures every way a stored grade can fall behind its
// source, without a join the settle scan would have to evaluate per row:
//
//   - Never graded (a fresh ingest, or a session that predates signals): the column defaults
//     true, so it is due from creation until the first grade.
//   - Graded before the session settled (a rebuild that ran refreshSignalsTx while the
//     session was still live, so its outcome was not yet stable): that refresh left the flag
//     set because the session was not idleLongEnough, so this tick re-grades it once settled.
//
// (A projection change needs no flag write of its own: a rebuild always ends by grading or,
// for a live session, leaving the flag set, so the grade can never silently trail the
// projection.)
//
// It drains the whole due backlog in bounded batches through two keyset scans, each resuming
// strictly after the last row of the previous batch, and a session drops out of its scan the
// moment it is graded, so the pass reads only the due rows via a partial index, O(D_due) per
// wake rather than O(settled history):
//   - the settled-by-idle tail in (ended_at, id) order (idx_sessions_signals_stale), and
//   - terminal stale sessions in id order (idx_sessions_terminal_stale), which are gradeable
//     regardless of ended_at and so cannot ride the settled drain's ended_at cursor (a terminal
//     transcript can carry a NULL ended_at yet still be gradeable, so keying the terminal drain
//     on id keeps the due-query scope matched to gatherSignalFacts).
//
// A session that is both settled and terminal is graded by whichever scan reaches it first and
// skipped by the other (its signals_stale is cleared), so it is never graded twice. Each session
// is refreshed in its own transaction so one slow session never holds a broad lock, cancellation
// stops the drain between sessions, and it returns how many it refreshed.
func (s *Store) RefreshSettledSignals(ctx context.Context) (int, error) {
	total := 0
	// Drain the settled-by-idle backlog first, keyset-paging the settled-and-stale tail in
	// (ended_at, id) order. The zero time sorts before every real ended_at, so the first
	// batch starts at the oldest settled session and the cursor only moves forward.
	var afterEnded time.Time
	var afterID int64
	for {
		ids, lastEnded, lastID, err := s.dueSettledBatch(ctx, afterEnded, afterID, settledSignalBatch)
		if err != nil {
			return total, err
		}
		n, err := s.refreshBatch(ctx, ids)
		total += n
		if err != nil {
			return total, err
		}
		if len(ids) < settledSignalBatch {
			break // a short batch means the settled backlog is drained
		}
		afterEnded, afterID = lastEnded, lastID
	}
	// Then drain terminal sessions, which are gradeable regardless of ended_at and so ride
	// their own id-keyed cursor rather than the settled drain's (ended_at, id) one (a terminal
	// transcript can carry a NULL ended_at yet still be gradeable). A terminal session the
	// settled drain already graded has its signals_stale cleared, so this fresh query skips it:
	// no session is graded twice.
	var afterTermID int64
	for {
		ids, lastID, err := s.dueTerminalBatch(ctx, afterTermID, settledSignalBatch)
		if err != nil {
			return total, err
		}
		n, err := s.refreshBatch(ctx, ids)
		total += n
		if err != nil {
			return total, err
		}
		if len(ids) < settledSignalBatch {
			return total, nil // a short batch means the terminal backlog is drained
		}
		afterTermID = lastID
	}
}

// refreshBatch refreshes each session id in its own transaction, returning how many it
// refreshed. It stops at the first error (returning the count so far) and checks cancellation
// between sessions, so one slow or failing session neither holds a broad lock nor loses the
// count of work already committed. It is the shared body of both settle drains (settled and
// terminal).
func (s *Store) refreshBatch(ctx context.Context, ids []int64) (int, error) {
	n := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if err := s.RefreshSessionSignals(ctx, id); err != nil {
			return n, fmt.Errorf("refresh settled session %d: %w", id, err)
		}
		n++
	}
	return n, nil
}

// dueSettledBatch returns up to limit due settled session ids strictly after the
// (afterEnded, afterID) keyset cursor, in (ended_at, id) order, with the cursor to resume
// from (the last row's ended_at and id). A session is due here when it is settled (ended_at
// at least abandonedIdleMinutes in the past) and signals_stale is set; both are columns of
// sessions, so the partial index idx_sessions_signals_stale serves the whole predicate and the
// scan visits only due rows. Terminal sessions are drained separately (dueTerminalBatch), since
// they are gradeable regardless of ended_at and so cannot ride this ended_at-keyed cursor. The
// ids are read up front and the query's connection is released (deferred Close) before the
// caller refreshes them, so the scan does not contend with the per-session refresh transactions.
func (s *Store) dueSettledBatch(ctx context.Context, afterEnded time.Time, afterID int64, limit int) ([]int64, time.Time, int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT s.id, s.ended_at
		   FROM sessions s
		  WHERE s.signals_stale
		    AND s.ended_at IS NOT NULL
		    AND s.ended_at < now() - make_interval(mins => $1)
		    AND (s.ended_at, s.id) > ($3, $4)
		  ORDER BY s.ended_at, s.id
		  LIMIT $2`,
		abandonedIdleMinutes, limit, afterEnded, afterID)
	if err != nil {
		return nil, afterEnded, afterID, fmt.Errorf("select settled sessions for signal refresh: %w", err)
	}
	defer rows.Close()
	var ids []int64
	lastEnded, lastID := afterEnded, afterID
	for rows.Next() {
		var id int64
		var ended time.Time
		if err := rows.Scan(&id, &ended); err != nil {
			return nil, afterEnded, afterID, fmt.Errorf("scan settled session id: %w", err)
		}
		ids = append(ids, id)
		lastEnded, lastID = ended, id
	}
	if err := rows.Err(); err != nil {
		return nil, afterEnded, afterID, fmt.Errorf("iterate settled sessions: %w", err)
	}
	return ids, lastEnded, lastID, nil
}

// dueTerminalBatch returns up to limit due terminal session ids strictly after the afterID
// keyset cursor, in id order, with the last id to resume from. A session is due here when it
// is stale and terminal: the client declared it finished (`akari sync --finalize`), so it is
// gradeable now regardless of its ended_at, which gatherSignalFacts already treats as
// idle-long-enough. This is a separate drain from dueSettledBatch precisely because a terminal
// transcript may carry no parseable timestamp (a NULL ended_at) yet still have gradeable
// messages: the settled drain's (ended_at, id) cursor cannot order a NULL, so it would strand
// such a session ungraded whenever the explicit finalize call was missed. Keying on id alone
// (served by the partial idx_sessions_terminal_stale) covers every terminal stale row, NULL
// ended_at included, so the due-query scope matches the derivation scope. The terminal set is
// tiny and short-lived (a grade clears signals_stale), so this scan reads a handful of rows.
// A terminal session that is also settled is graded by whichever drain reaches it first and
// dropped from the other by the cleared flag, so it is never graded twice.
func (s *Store) dueTerminalBatch(ctx context.Context, afterID int64, limit int) ([]int64, int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT s.id
		   FROM sessions s
		  WHERE s.signals_stale
		    AND s.terminal
		    AND s.id > $2
		  ORDER BY s.id
		  LIMIT $1`,
		limit, afterID)
	if err != nil {
		return nil, afterID, fmt.Errorf("select terminal sessions for signal refresh: %w", err)
	}
	defer rows.Close()
	var ids []int64
	lastID := afterID
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, afterID, fmt.Errorf("scan terminal session id: %w", err)
		}
		ids = append(ids, id)
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return nil, afterID, fmt.Errorf("iterate terminal sessions: %w", err)
	}
	return ids, lastID, nil
}

// settledSignalBatch bounds one due-session query and the run of per-session refreshes
// behind it. It is a var so a test can shrink it to exercise the multi-batch keyset drain
// without seeding thousands of sessions (see SetSettledSignalBatch).
var settledSignalBatch = 500

// SessionSignalsByID reads a session's up-to-date stored signals. A session with no usable
// row reads as an unknown, unscored result rather than an error, so the session page renders
// a neutral state instead of a stale or missing grade. A row is usable only when the session
// is not flagged signals_stale, so the header and the fleet aggregates gate on the same flag
// and agree on exactly which grades count. refreshSignalsTx leaves the flag set when it grades
// a still-live session, so a not-yet-stable outcome (abandoned versus unknown turns on the
// idle gap) never reaches a reader before the settle tick pins it.
//
// Gating on the flag rather than a refreshed_at >= updated_at comparison is deliberate.
// updated_at also moves on metadata-only writes (an announce re-announce, an owner
// reassignment) that leave the grade valid, so keying reads on it would strand those grades
// unread while the settle tick, which keys on the flag, never revisits them.
func (s *Store) SessionSignalsByID(ctx context.Context, sessionID int64) (SessionSignals, error) {
	return s.sessionSignals(ctx, s.Pool, sessionID)
}

// sessionSignals is SessionSignalsByID over an arbitrary querier, so the audit bundle
// can read the signals row in the same snapshot as the costs it is judged beside.
func (s *Store) sessionSignals(ctx context.Context, q querier, sessionID int64) (SessionSignals, error) {
	var sig SessionSignals
	err := q.QueryRow(ctx,
		`SELECT sig.session_id, sig.outcome, sig.outcome_confidence, sig.score, sig.grade,
		        sig.tool_calls, sig.tool_failures, sig.tool_retries, sig.edit_churn, sig.longest_failure_streak,
		        sig.prompt_count, sig.short_prompt_count, sig.duplicate_prompt_count, sig.no_code_context_count, sig.unstructured_start,
		        sig.peak_context_tokens, sig.context_reset_count,
		        sig.assistant_turns, sig.thinking_turns, sig.thinking_tail_tokens, sig.thinking_peak_tokens
		   FROM session_signals sig
		   JOIN sessions s ON s.id = sig.session_id
		  WHERE sig.session_id = $1 AND `+signalsCurrent(), sessionID).Scan(
		&sig.SessionID, &sig.Outcome, &sig.OutcomeConfidence, &sig.Score, &sig.Grade,
		&sig.ToolCalls, &sig.ToolFailures, &sig.ToolRetries, &sig.EditChurn, &sig.LongestFailureStreak,
		&sig.PromptCount, &sig.ShortPromptCount, &sig.DuplicatePromptCount, &sig.NoCodeContextCount, &sig.UnstructuredStart,
		&sig.PeakContextTokens, &sig.ContextResetCount,
		&sig.AssistantTurns, &sig.ThinkingTurns, &sig.ThinkingTailTokens, &sig.ThinkingPeakTokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionSignals{SessionID: sessionID, Outcome: string(quality.OutcomeUnknown), OutcomeConfidence: string(quality.ConfLow)}, nil
	}
	if err != nil {
		return SessionSignals{}, fmt.Errorf("read signals for session %d: %w", sessionID, err)
	}
	return sig, nil
}
