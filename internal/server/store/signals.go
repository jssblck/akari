package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/quality"
)

// abandonedIdleMinutes is how long a session whose last substantive turn was the
// user's must stay quiet before its outcome reads "abandoned" rather than "unknown".
// Below it the session may simply be mid-conversation, so the classifier withholds the
// verdict (see quality.Classify); a historical import, long past this window, settles.
const abandonedIdleMinutes = 30

// SessionSignals is a session's stored behavioral signals: its outcome, its quality
// score and grade (nil when unscored), and the tool-health counts the score is built
// from. It is the read shape of the session_signals row, derived from the session's
// own projection and materialized by the settle pass once the session settles, or
// re-derived on reparse.
type SessionSignals struct {
	SessionID            int64
	Version              int
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
	// HygieneMeasured is true only when the row's stored prompt-hygiene counts were derived at
	// the running quality.PromptFactsVersion. The classifier version is deliberately separate
	// from the scoring quality.Version (see quality.PromptFactsVersion), so a row still at the
	// current signals_version can carry hygiene counts from a superseded classifier until the
	// reparse re-derives them. The read sets this from session_signals.prompt_facts_version, and
	// HasHygieneSignal gates on it, so a stale-classifier count never surfaces as a signal.
	HygieneMeasured bool
	// Context-health figures describe resource load, not the agent's work, so like the
	// hygiene counts they ride alongside the score without feeding it. Both are nil when
	// the session had no usage to measure, so the UI can tell "unmeasured" apart from a
	// measured zero. PeakContextTokens is the heaviest single-turn context the session
	// reached; ContextResetCount is how many inferred context resets (compactions or
	// clears) it went through.
	PeakContextTokens *int64
	ContextResetCount *int
}

// Scored reports whether the session carries a score and grade, so the UI can show a
// grade tile or fall back to the outcome alone for an unscored (unknown, no-signal)
// session.
func (s SessionSignals) Scored() bool { return s.Score != nil && s.Grade != nil }

// HasToolActivity reports whether the session ran any tools, so the UI can omit the
// tool-health detail for a pure-conversation session that has none.
func (s SessionSignals) HasToolActivity() bool { return s.ToolCalls > 0 }

// HasHygieneSignal reports whether any prompt-hygiene signal fired, so the UI can omit
// the input readout for a session whose prompts were all clean. It is false when the row's
// hygiene is not measured at the current classifier version (HygieneMeasured), so the session
// page never shows a count a superseded classifier produced: the block reads as unmeasured until
// the reparse re-derives the facts, the same way the fleet hygiene aggregate excludes the row.
func (s SessionSignals) HasHygieneSignal() bool {
	return s.HygieneMeasured && (s.ShortPromptCount > 0 || s.DuplicatePromptCount > 0 ||
		s.NoCodeContextCount > 0 || s.UnstructuredStart)
}

// HasContextHealth reports whether the session had usage to measure, so the UI can show the
// context readout only when there is a real figure rather than a blank stand-in. Peak and
// reset count are populated together, so testing the peak is enough.
func (s SessionSignals) HasContextHealth() bool { return s.PeakContextTokens != nil }

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
	// promptFactsReady is false when the session still has a human prompt whose hygiene facts
	// are NULL: a pre-migration message the Epoch reparse has not yet re-inserted with facts.
	// refreshSignalsTx leaves such a session stale and ungraded rather than record an all-zero
	// hygiene row that would mask the real signal until the reparse catches up.
	promptFactsReady bool

	// Context-health facts, computed from the session's ordered usage. Both are nil when
	// there was no usage to measure (so the row stores NULL, not a misleading zero);
	// peakContextTokens is the heaviest single-turn context and contextResets is the
	// inferred compaction/clear count.
	peakContextTokens *int64
	contextResets     *int
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
	hygiene, promptCount, ready, err := gatherPromptHygiene(ctx, tx, sessionID)
	if err != nil {
		return signalFacts{}, err
	}
	f.hygiene = hygiene
	f.promptCount = promptCount
	f.promptFactsReady = ready

	// Context-health facts. Read from the same projection but from usage_events rather
	// than messages, so they live in their own pass over the session's ordered turns.
	if err := gatherContextHealth(ctx, tx, sessionID, &f); err != nil {
		return signalFacts{}, err
	}
	return f, nil
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
// ready is false when any human prompt lacks current facts: either NULL facts (a message written
// before migration 0022 added the columns, not yet re-inserted by the Epoch reparse) or facts at an
// older prompt_facts_version (classified under a superseded ClassifyPrompt, not yet re-derived). The
// columns are filled at insert and cannot backfill or re-derive on their own, so a session in either
// state would otherwise aggregate to hygiene the current classifier never produced. The caller uses
// ready to leave such a session ungraded until the reparse re-derives its facts, rather than record a
// count that reads as measured. A session with no human prompts (automation) has nothing to derive,
// so it reads ready and grades to an honest empty hygiene.
func gatherPromptHygiene(ctx context.Context, tx pgx.Tx, sessionID int64) (quality.PromptHygiene, int, bool, error) {
	var (
		h           quality.PromptHygiene
		promptCount int
		stale       bool
	)
	if err := tx.QueryRow(ctx,
		`WITH prompts AS (
		   SELECT ordinal, prompt_short, prompt_no_code, prompt_bare_greeting, prompt_digest, prompt_facts_version
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
		                    AND ordinal = (SELECT min(ordinal) FROM prompts)), FALSE),
		   coalesce(bool_or(prompt_digest IS NULL OR prompt_facts_version IS DISTINCT FROM $2), FALSE)
		   FROM prompts`,
		sessionID, quality.PromptFactsVersion).Scan(&promptCount, &h.Short, &h.NoCodeContext, &h.Duplicate, &h.UnstructuredStart, &stale); err != nil {
		return quality.PromptHygiene{}, 0, false, fmt.Errorf("gather prompt hygiene for session %d: %w", sessionID, err)
	}
	return h, promptCount, !stale, nil
}

// refreshSignalsTx recomputes a session's signals from its projection and UPSERTs the
// session_signals row, inside the caller's transaction. It is driven by the settle pass
// (each due session in its own transaction) and by a reparse (in the reparse transaction,
// so the signals commit atomically with the projection they summarize; the rows it reads
// are already written and visible in-txn). It is a whole-session recompute, not an
// incremental fold: the signals depend on cross-message order (retry runs, failure streaks,
// the last word), which a per-region delta cannot carry, which is also why it is not run on
// the incremental append path.
func refreshSignalsTx(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	// Bow out if a newer binary has already won the version marker. During a rolling deploy an old
	// binary at quality.Version N-1 and a new one at N share the database, and the new one advances
	// signals_reconciled_version to N as it re-grades. Reading that marker here, in the transaction
	// that already holds the session lock, lets an old settle pass (or an old reparse) that raced this
	// far see the newer marker and leave the row and its signals_stale flag alone rather than overwrite
	// a fresh N grade with an N-1 one and clear the flag, which would hide the row from the N binary's
	// readers with no later reconcile to mark it due again. This is the reviewer-prescribed recheck of
	// the marker under the session transaction, the airtight half the reconcile gate cannot cover on
	// its own: reconcileStaleVersionsIfNeeded stops an old binary re-marking rows once the marker is
	// ahead, but a pass that had already selected a due session before the marker moved still reaches
	// this write, and only rereading the marker here can stop it.
	//
	// The read is plain (MVCC last-committed), and that suffices. An N-version row is only ever written
	// by an N binary, whose settle pass advances the marker to N before it drains (RefreshSettledSignals
	// reconciles first), so any committed N row implies a committed marker at least N. An old binary
	// that still reads a marker at or below its own version is therefore provably not racing a committed
	// N row it could clobber. If instead it reads a stale marker and grades at N-1, the N binary's later
	// reconcile (its mark-stale UPDATE contends on this same session row lock, so it runs strictly
	// before or after this write, never interleaved) re-marks the row stale and the N binary re-grades
	// it. Either way the corpus converges on the newest binary's grade.
	var marker int
	if err := tx.QueryRow(ctx,
		`SELECT signals_reconciled_version FROM parse_meta WHERE id = TRUE`).Scan(&marker); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read signals marker for session %d: %w", sessionID, err)
		}
		// No singleton row (a database before migration 0013): nothing has reconciled, so no newer
		// binary can have won and marker stays 0, below any real version.
	}
	if marker > quality.Version {
		return nil // superseded by a newer binary; leave the row (and its signals_stale flag) for it
	}
	f, err := gatherSignalFacts(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if !f.promptFactsReady {
		// A session whose human prompts lack current facts: either NULL facts (a message written
		// before migration 0022 added the columns, which cannot backfill on ALTER) or facts at a
		// superseded prompt_facts_version (classified by an older ClassifyPrompt). Grading now would
		// store an all-zero or version-mixed hygiene row and read as measured until something re-graded
		// it, so this leaves the session ungraded: only the Epoch bump paired with each classifier
		// change re-inserts every message through it (see epoch.go, projection.go), so it is the
		// reparse, not this settle pass, that first records this session's hygiene.
		//
		// It also drops the session out of the settle-due set, but ONLY when doing so cannot expose a
		// stale grade. Re-selecting a permanently ungradeable session every wake would be pure waste:
		// gatherSignalFacts has already scanned this session's messages, tool_calls, and usage_events
		// before reaching this guard, and nothing the settle pass does can fill the facts (only a
		// reparse re-inserts the messages), so a session stuck here, a deterministic parser failure the
		// epoch cannot rebuild is the worst case, would be re-scanned on every tick forever,
		// O(ticks * history) of unchanged work that grows with the ungradeable set.
		//
		// The clear is guarded by NOT EXISTS a current-version session_signals row, because clearing
		// signals_stale unconditionally would break projection consistency. A session can hold a row at
		// the current signals_version (it graded cleanly once) and later have its facts go stale, when a
		// PromptFactsVersion bump supersedes the classifier its messages were tagged under, while new
		// messages or tool calls or usage since that grade already set signals_stale = true through
		// applyAggregates. That stored row now reflects the pre-append projection. If this guard cleared
		// signals_stale, the row would pass the NOT signals_stale AND signals_version = quality.Version
		// read gate again and serve a stale outcome, score, and tool-health as if current, until a
		// reparse happened to re-derive it. So when such a row exists the clear affects zero rows: the
		// flag stays true, the stale row stays hidden, and the session stays due, re-scanned until the
		// paired epoch reparse re-derives its facts and re-grades it (a bounded rollout window, not the
		// forever loop, because it ends when the reparse reaches the session).
		//
		// When no current-version row exists there is nothing to expose, so the clear fires and the
		// session leaves the due set. That is the ungradeable case the guard exists for: a session that
		// never graded (no row) or graded only under a superseded signals_version (which the read gate
		// already hides) is safe to drop. The reparse that does fill the facts re-marks it due through
		// applyAggregates and grades it with facts now current, and any later append re-marks it the same
		// way, so a droppable session is re-graded exactly when it becomes gradeable, never polled until then.
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET signals_stale = false
			  WHERE id = $1
			    AND NOT EXISTS (SELECT 1 FROM session_signals
			                     WHERE session_id = $1 AND signals_version = $2)`,
			sessionID, quality.Version); err != nil {
			return fmt.Errorf("clear signals_stale for ungradeable session %d: %w", sessionID, err)
		}
		return nil
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
		   (session_id, signals_version, outcome, outcome_confidence, score, grade,
		    tool_calls, tool_failures, tool_retries, edit_churn, longest_failure_streak,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start,
		    peak_context_tokens, context_reset_count, prompt_facts_version,
		    refreshed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19, now())
		 ON CONFLICT (session_id) DO UPDATE SET
		   signals_version = EXCLUDED.signals_version,
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
		   prompt_facts_version = EXCLUDED.prompt_facts_version,
		   refreshed_at = now()`,
		sessionID, quality.Version, string(outcome), string(conf), scoreArg, gradeArg,
		f.toolCalls, f.toolFailures, f.toolRetries, f.editChurn, f.longestFailureStreak,
		f.promptCount, f.hygiene.Short, f.hygiene.Duplicate, f.hygiene.NoCodeContext, f.hygiene.UnstructuredStart,
		f.peakContextTokens, f.contextResets, quality.PromptFactsVersion)
	if err != nil {
		return fmt.Errorf("upsert signals for session %d: %w", sessionID, err)
	}
	// Clear the settle-pass due flag now that the grade matches the projection, but only if
	// the session's outcome is stable, meaning idleLongEnough holds (it has settled past the
	// idle window, or the client declared it terminal). A reparse runs refreshSignalsTx on
	// whatever it rebuilds, including a still-live session whose outcome is not yet stable
	// (abandoned versus unknown turns on the idle gap), so leaving signals_stale set there
	// keeps the settle pass on the hook to re-grade once the session crosses the idle
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
// standalone form the settle pass (RefreshSettledSignals) and the tests use; the reparse
// path calls refreshSignalsTx inside its existing transaction instead, so the signals commit
// with the projection rather than in a second round trip.
func (s *Store) RefreshSessionSignals(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		return refreshSignalsTx(ctx, tx, sessionID)
	})
}

// RefreshSettledSignals recomputes signals for every settled session marked stale. It is the
// production path that materializes signals: the append path no longer refreshes on catch-up
// (see AdvanceProjection), so a session's signals are computed once here, after it has been
// idle past the abandoned threshold, off the ingest hot path.
//
// A session is due when signals_stale is set AND it is either settled (ended_at at least
// abandonedIdleMinutes in the past) or terminal (the client declared it finished via
// `akari sync --finalize`, so it is gradeable now without waiting out the idle window). The
// flag is the single-table marker that replaces a cross-table due predicate: applyAggregates
// and the reparse reset set it whenever the projection moves, and refreshSignalsTx clears it
// only when it grades a settled-or-terminal session. So it captures every way a stored grade
// can fall behind its source, without a join the settle scan would have to evaluate per row:
//
//   - Never graded (a fresh ingest, or a session that predates signals): the column defaults
//     true, so it is due from creation until the first settle grades it.
//   - Graded before the projection last changed (a later chunk of a multi-upload historical
//     session, whose ended_at stays far in the past so it looks long settled): the appended
//     region set the flag again, so the stale partial grade is re-derived.
//   - Graded before the session settled (a reparse that ran refreshSignalsTx while the session
//     was still live, so its outcome was not yet stable): that refresh left the flag set
//     because the session was not idleLongEnough, so the settle pass re-grades it once settled.
//
// A stale signals_version is the one case the flag does not cover on its own (a version bump
// changes no projection), so reconcileStaleVersionsIfNeeded marks those rows before the drain. It
// runs the inequality scan once per quality.Version change, gated on a parse_meta marker, so a
// steady-state wake never pays it.
//
// It drains the whole due backlog in bounded batches, keyset-paging the settled-and-stale tail
// once in (ended_at, id) order: each batch resumes strictly after the last row of the previous
// one, and a session drops out of the settle index the moment it is graded, so the pass reads
// only the due rows via the partial index (idx_sessions_signals_stale), O(D_due) per wake
// rather than O(settled history). Each session is refreshed in its own transaction so one slow
// session never holds a broad lock, cancellation stops the drain between sessions, and it
// returns how many it refreshed.
func (s *Store) RefreshSettledSignals(ctx context.Context) (int, error) {
	// A version bump leaves current signals_stale=false rows whose stored version is behind; mark
	// them stale first so they drain through the same path as a projection change. This is gated
	// to run once per quality.Version change (see reconcileStaleVersionsIfNeeded), so a normal
	// wake with the marker already current pays one O(1) read, not the inequality scan.
	if err := s.reconcileStaleVersionsIfNeeded(ctx); err != nil {
		return 0, err
	}
	// The zero time sorts before every real ended_at, so the first batch starts at the
	// oldest settled session and the cursor only moves forward from there.
	var afterEnded time.Time
	var afterID int64
	total := 0
	for {
		ids, lastEnded, lastID, err := s.dueSettledBatch(ctx, afterEnded, afterID, settledSignalBatch)
		if err != nil {
			return total, err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			if err := s.RefreshSessionSignals(ctx, id); err != nil {
				return total, fmt.Errorf("refresh settled session %d: %w", id, err)
			}
			total++
		}
		if len(ids) < settledSignalBatch {
			return total, nil // a short batch means the due backlog is drained
		}
		afterEnded, afterID = lastEnded, lastID
	}
}

// reconcileStaleVersionsIfNeeded runs the version reconcile at most once per quality.Version
// change. The reconcile scans for signals_version <> current, an inequality no index can seek, so
// running it on every settle wake would make idle maintenance grow with the whole signals table
// rather than the due tail. A single-row marker in parse_meta records the version the corpus was
// last reconciled at: in steady state this is one O(1) singleton read that finds the marker
// current and skips the scan. A quality.Version bump ships in a new binary, so the first settle
// pass after the upgrade sees the marker behind, runs the reconcile once to flag every
// stale-version row, and advances the marker; every later pass drains those rows through the
// signals_stale index without rescanning.
//
// The marker read, the stale-marking side effect, and the marker advance all run in ONE transaction
// that holds the parse_meta row lock (SELECT ... FOR UPDATE), which is what keeps a rolling deploy
// correct. During a rollout an old binary at quality.Version N-1 and a new one at N share the
// database. The lock serializes their reconciles and forces the version recheck to happen while the
// lock is held, so whichever binary runs second re-reads the marker the winner wrote and, finding it
// at or past its own version, does nothing. A plain marker compare-and-set is not enough: without
// the lock the version check and the stale-marking UPDATE are separate steps, so an old N-1 binary
// that read a behind marker could run the reconcile AFTER a new N binary had already marked,
// advanced, and re-graded, re-marking the fresh N rows stale so the old settle drain overwrites them
// with N-1 output. The compare-and-set stops the marker regressing but not that late side effect;
// the lock stops both. Running the side effect and the advance in the same transaction also makes
// the pair atomic: a crash rolls both back, so the next pass repeats the reconcile cleanly.
func (s *Store) reconcileStaleVersionsIfNeeded(ctx context.Context) error {
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var reconciled int
		if err := tx.QueryRow(ctx,
			`SELECT signals_reconciled_version FROM parse_meta WHERE id = TRUE FOR UPDATE`).Scan(&reconciled); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // the migration seeds the singleton; a missing row means nothing to lock or reconcile against
			}
			return fmt.Errorf("lock signals_reconciled_version: %w", err)
		}
		if reconciled >= quality.Version {
			return nil // already reconciled at this version or a newer one; never step the marker back
		}
		if err := s.reconcileStaleVersions(ctx, tx); err != nil {
			return err
		}
		// The recheck above ran under the row lock, so the marker is provably still behind and no other
		// binary can move it while this transaction holds the lock: a plain advance is safe here.
		if _, err := tx.Exec(ctx,
			`UPDATE parse_meta SET signals_reconciled_version = $1, updated_at = now() WHERE id = TRUE`,
			quality.Version); err != nil {
			return fmt.Errorf("mark signals reconciled at version %d: %w", quality.Version, err)
		}
		return nil
	}); err != nil {
		// Wrap the transaction result too, so a begin or commit failure (which never reaches the
		// callback and so is not wrapped inside it) still reaches RefreshSettledSignals named.
		return fmt.Errorf("reconcile stale signal versions: %w", err)
	}
	return nil
}

// reconcileStaleVersions marks any session whose stored signals row is at a superseded
// quality.Version as stale, so a version bump re-grades incrementally through the same
// signals_stale drain as a projection change would. A version bump changes no projection, so the
// projection-maintained flag cannot catch it; this closes that one gap. reconcileStaleVersionsIfNeeded
// gates it to run once per version change, since the signals_version <> current test is an
// inequality scan the settle loop must not repeat on every wake. It runs on the caller's transaction
// (tx) so the mark, the marker read that gated it, and the marker advance are one atomic, row-locked
// unit; see reconcileStaleVersionsIfNeeded for why the lock, not just a marker CAS, is required.
//
// It deliberately marks EVERY stale-version session, with no `AND NOT s.signals_stale` skip, even
// though re-marking an already-due session is a no-op on the flag value. The write is not for the
// value, it is for the row LOCK: a rolling deploy runs an old settle pass at quality.Version N-1
// against this new binary's N reconcile, and the serialization the marker gate in refreshSignalsTx
// relies on only holds if the reconcile locks the session row. Skipping already-stale sessions would
// leave a hole: an old pass that had already selected an already-due stale-version session could lock
// it, read the pre-advance marker, write an N-1 row, and clear signals_stale, all while this reconcile
// (having skipped that row) advances the marker and commits. The marker is then current so this
// reconcile never runs again, and the session is left at a stale-version row that no reader counts
// (the read gate wants quality.Version) and no drain re-grades (signals_stale is now false): stuck
// ungraded until its next projection change. Locking every stale-version row closes that: an old pass
// either locks first (then this UPDATE waits, re-reads the still-stale-version row after the old pass
// commits its N-1 grade, and re-marks it, so the N drain re-grades) or this UPDATE locks first (then
// the old pass's lockSession waits, reads the advanced marker, and bows out). Either order converges.
func (s *Store) reconcileStaleVersions(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx,
		`UPDATE sessions s SET signals_stale = true
		   FROM session_signals sig
		  WHERE sig.session_id = s.id
		    AND sig.signals_version <> $1`, quality.Version); err != nil {
		return fmt.Errorf("reconcile stale signal versions: %w", err)
	}
	return nil
}

// dueSettledBatch returns up to limit due settled session ids strictly after the
// (afterEnded, afterID) keyset cursor, in (ended_at, id) order, with the cursor to resume
// from (the last row's ended_at and id). A session is due when it is stale and either
// settled (ended_at at least abandonedIdleMinutes in the past) OR terminal (the client
// declared it finished via `akari sync --finalize`, so it is gradeable now regardless of
// the idle window; see RefreshSettledSignals). All three are columns of sessions:
// idx_sessions_signals_stale serves the settled disjunct as an ended_at range seek, and the
// partial idx_sessions_terminal_stale carries the terminal disjunct so a just-ended terminal
// row (which sits past the settled cutoff) is found by an index scan rather than a walk of the
// whole stale tail. Ordering by (ended_at, id) keeps the keyset total: a terminal session's
// recent ended_at simply sorts it toward the end of the drain, visited once like any other.
// The ids are read up front and the query's connection is released (deferred Close) before the
// caller refreshes them, so the scan does not contend with the per-session refresh transactions.
func (s *Store) dueSettledBatch(ctx context.Context, afterEnded time.Time, afterID int64, limit int) ([]int64, time.Time, int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT s.id, s.ended_at
		   FROM sessions s
		  WHERE s.signals_stale
		    AND s.ended_at IS NOT NULL
		    AND (s.terminal OR s.ended_at < now() - make_interval(mins => $1))
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

// settledSignalBatch bounds one due-session query and the run of per-session refreshes
// behind it. It is a var so a test can shrink it to exercise the multi-batch keyset drain
// without seeding thousands of sessions (see SetSettledSignalBatch).
var settledSignalBatch = 500

// SessionSignalsByID reads a session's current-version, up-to-date stored signals. A session
// with no usable row reads as an unknown, unscored result rather than an error, so the session
// page renders a neutral state instead of a stale or missing grade. A row is usable only when
// it is at the current signals_version AND the session is not flagged signals_stale, so the
// header and the fleet aggregates gate on the same flag and agree on exactly which grades count.
// signals_stale is set whenever the projection moves (applyAggregates and the reparse reset), so
// a session that gained an appended region after its last grade reads as unmeasured until the
// settle pass re-grades it, rather than showing a grade for an earlier, smaller session. The
// flag also covers the pre-settle case: refreshSignalsTx leaves it set when it grades a
// still-live session, so a not-yet-stable outcome (abandoned versus unknown turns on the idle
// gap) never reaches a reader before the settle pass pins it.
//
// Gating on the flag rather than a refreshed_at >= updated_at comparison is deliberate.
// updated_at also moves on metadata-only writes (an announce re-announce, an owner
// reassignment) that leave the grade valid, so keying reads on it would strand those grades
// unread while the settle pass, which keys on the flag, never revisits them. The flag is set at
// exactly the projection-change sites, so it is the precise "grade is behind its source" signal
// that updated_at is not.
func (s *Store) SessionSignalsByID(ctx context.Context, sessionID int64) (SessionSignals, error) {
	var sig SessionSignals
	var promptFactsVersion int
	err := s.Pool.QueryRow(ctx,
		`SELECT sig.session_id, sig.signals_version, sig.outcome, sig.outcome_confidence, sig.score, sig.grade,
		        sig.tool_calls, sig.tool_failures, sig.tool_retries, sig.edit_churn, sig.longest_failure_streak,
		        sig.prompt_count, sig.short_prompt_count, sig.duplicate_prompt_count, sig.no_code_context_count, sig.unstructured_start,
		        sig.peak_context_tokens, sig.context_reset_count, sig.prompt_facts_version
		   FROM session_signals sig
		   JOIN sessions s ON s.id = sig.session_id
		  WHERE sig.session_id = $1 AND sig.signals_version = $2 AND NOT s.signals_stale`, sessionID, quality.Version).Scan(
		&sig.SessionID, &sig.Version, &sig.Outcome, &sig.OutcomeConfidence, &sig.Score, &sig.Grade,
		&sig.ToolCalls, &sig.ToolFailures, &sig.ToolRetries, &sig.EditChurn, &sig.LongestFailureStreak,
		&sig.PromptCount, &sig.ShortPromptCount, &sig.DuplicatePromptCount, &sig.NoCodeContextCount, &sig.UnstructuredStart,
		&sig.PeakContextTokens, &sig.ContextResetCount, &promptFactsVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionSignals{SessionID: sessionID, Outcome: string(quality.OutcomeUnknown), OutcomeConfidence: string(quality.ConfLow)}, nil
	}
	if err != nil {
		return SessionSignals{}, fmt.Errorf("read signals for session %d: %w", sessionID, err)
	}
	// The outcome, score, and tool-health counts are gated on signals_version above and read as
	// current. The prompt-hygiene counts carry their own quality.PromptFactsVersion (they aggregate
	// the messages.prompt_* facts), so a row can be at the current signals_version yet hold hygiene
	// from a superseded classifier until the reparse re-derives it. Mark hygiene measured only when
	// its version matches, so HasHygieneSignal (and the session page it drives) reads a stale count
	// as unmeasured rather than as current. The row is not hidden wholesale: the non-hygiene signals
	// do not depend on the classifier, so suppressing them would drop correct data.
	sig.HygieneMeasured = promptFactsVersion == quality.PromptFactsVersion
	return sig, nil
}
