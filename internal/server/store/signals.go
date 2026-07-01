package store

import (
	"context"
	"errors"
	"fmt"

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
// own projection and rebuilt on catch-up or reparse.
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

	// hygiene is computed in Go from the session's ordered human prompts rather than in
	// SQL: the rules are text heuristics (word counts, code detection, verbatim repeats)
	// that read far clearer in the tested quality package than as a window-function query.
	hygiene quality.PromptHygiene
	// promptCount is the classifier's base: the count of non-empty human prompts it saw,
	// stored so the cohort aggregate divides the hygiene counts by exactly the set they
	// came from rather than by user_message_count (which can include empty-text turns).
	promptCount int
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
	// turns require non-empty content, so an empty tool-plumbing turn does not count as
	// the last word. idle is measured against the session's last activity.
	err = tx.QueryRow(ctx,
		`SELECT s.user_message_count,
		        coalesce((SELECT max(ordinal) FROM messages WHERE session_id = $1 AND role = 'assistant' AND content <> ''), -1),
		        coalesce((SELECT max(ordinal) FROM messages WHERE session_id = $1 AND role = 'user' AND content <> ''), -1),
		        (s.ended_at IS NOT NULL AND s.ended_at < now() - make_interval(mins => $2))
		   FROM sessions s WHERE s.id = $1`,
		sessionID, abandonedIdleMinutes).Scan(
		&f.userMessages, &f.lastAssistantOrd, &f.lastUserOrd, &f.idleLongEnough)
	if err != nil {
		return signalFacts{}, fmt.Errorf("gather outcome facts for session %d: %w", sessionID, err)
	}

	// Prompt-hygiene facts. The human prompts in order, non-empty only (an empty turn is
	// tool plumbing, not a prompt), so the classifier never reads a blank as a terse
	// turn. role='user' is already the real-human-turn set (the Claude reducer drops
	// tool-result-only user entries), and ordinal order puts the opening prompt first so
	// the unstructured-start rule reads the right turn.
	prompts, err := gatherPromptTexts(ctx, tx, sessionID)
	if err != nil {
		return signalFacts{}, err
	}
	f.promptCount = len(prompts)
	f.hygiene = quality.ClassifyPromptHygiene(prompts)
	return f, nil
}

// gatherPromptTexts reads a session's human prompts in transcript order, dropping empties
// so a tool-plumbing turn does not read as a terse prompt. It is a small read (bounded by
// the session's human turns) and feeds quality.ClassifyPromptHygiene.
func gatherPromptTexts(ctx context.Context, tx pgx.Tx, sessionID int64) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT content FROM messages
		  WHERE session_id = $1 AND role = 'user' AND content <> ''
		  ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("gather prompts for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	var prompts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan prompt for session %d: %w", sessionID, err)
		}
		prompts = append(prompts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompts for session %d: %w", sessionID, err)
	}
	return prompts, nil
}

// refreshSignalsTx recomputes a session's signals from its projection and UPSERTs the
// session_signals row, inside the caller's transaction. It runs as the last step of a
// catch-up or a reparse (the rows it reads are already written and visible in-txn), so
// the signals commit atomically with the projection they summarize. It is a whole-
// session recompute, not an incremental fold: the signals depend on cross-message order
// (retry runs, failure streaks, the last word), which a per-region delta cannot carry.
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
		   (session_id, signals_version, outcome, outcome_confidence, score, grade,
		    tool_calls, tool_failures, tool_retries, edit_churn, longest_failure_streak,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start,
		    refreshed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now())
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
		   refreshed_at = now()`,
		sessionID, quality.Version, string(outcome), string(conf), scoreArg, gradeArg,
		f.toolCalls, f.toolFailures, f.toolRetries, f.editChurn, f.longestFailureStreak,
		f.promptCount, f.hygiene.Short, f.hygiene.Duplicate, f.hygiene.NoCodeContext, f.hygiene.UnstructuredStart)
	if err != nil {
		return fmt.Errorf("upsert signals for session %d: %w", sessionID, err)
	}
	return nil
}

// RefreshSessionSignals recomputes one session's signals in its own transaction. It is
// the standalone form the backfill (a reparse) and the tests use; the live parse and
// reparse paths call refreshSignalsTx inside their existing transaction instead, so the
// signals commit with the projection rather than in a second round trip.
func (s *Store) RefreshSessionSignals(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		return refreshSignalsTx(ctx, tx, sessionID)
	})
}

// SessionSignalsByID reads a session's stored signals. A session with no row yet (one
// ingested before signals existed and not yet reparsed, or still mid-first-parse) reads
// as an unknown, unscored result rather than an error, so the session page renders a
// neutral state instead of failing.
func (s *Store) SessionSignalsByID(ctx context.Context, sessionID int64) (SessionSignals, error) {
	var sig SessionSignals
	err := s.Pool.QueryRow(ctx,
		`SELECT session_id, signals_version, outcome, outcome_confidence, score, grade,
		        tool_calls, tool_failures, tool_retries, edit_churn, longest_failure_streak,
		        prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start
		   FROM session_signals WHERE session_id = $1`, sessionID).Scan(
		&sig.SessionID, &sig.Version, &sig.Outcome, &sig.OutcomeConfidence, &sig.Score, &sig.Grade,
		&sig.ToolCalls, &sig.ToolFailures, &sig.ToolRetries, &sig.EditChurn, &sig.LongestFailureStreak,
		&sig.PromptCount, &sig.ShortPromptCount, &sig.DuplicatePromptCount, &sig.NoCodeContextCount, &sig.UnstructuredStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionSignals{SessionID: sessionID, Outcome: string(quality.OutcomeUnknown), OutcomeConfidence: string(quality.ConfLow)}, nil
	}
	if err != nil {
		return SessionSignals{}, fmt.Errorf("read signals for session %d: %w", sessionID, err)
	}
	return sig, nil
}
