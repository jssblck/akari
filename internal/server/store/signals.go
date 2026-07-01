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
// the input readout for a session whose prompts were all clean.
func (s SessionSignals) HasHygieneSignal() bool {
	return s.ShortPromptCount > 0 || s.DuplicatePromptCount > 0 ||
		s.NoCodeContextCount > 0 || s.UnstructuredStart
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

	// hygiene is computed in Go from the session's ordered human prompts rather than in
	// SQL: the rules are text heuristics (word counts, code detection, verbatim repeats)
	// that read far clearer in the tested quality package than as a window-function query.
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

	// Prompt-hygiene facts, folded over the human prompts in order (non-empty only: an
	// empty turn is tool plumbing, not a prompt; role='user' is the real-human-turn set,
	// since the Claude reducer drops tool-result-only user entries; ordinal order puts the
	// opening prompt first so the unstructured-start rule reads the right turn).
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

// gatherPromptHygiene folds a session's human prompts into the hygiene signals and returns
// them with the prompt count. It reads the prompts in transcript order, dropping empties so
// a tool-plumbing turn does not read as a terse prompt, and folds each as it streams (see
// quality.PromptHygieneFolder), so no whole-session buffer of prompt bodies is held.
//
// The duplicate count is the one cross-prompt signal, and computing it exactly needs state
// proportional to the prompt count (a set of everything seen). Rather than hold that in Go,
// it is a database aggregate: over the same non-terse prompts (at least DuplicateMinWords
// words), the number of repeats beyond the first is the count minus the distinct normalized
// texts, where the normalization (lowercase, collapse whitespace, trim) mirrors the Go rule
// the folder's siblings use. This keeps peak memory on the refresh path bounded to the
// folder's O(1) state while preserving exact, whole-session duplicate detection.
func gatherPromptHygiene(ctx context.Context, tx pgx.Tx, sessionID int64) (quality.PromptHygiene, int, error) {
	rows, err := tx.Query(ctx,
		`SELECT content FROM messages
		  WHERE session_id = $1 AND role = 'user' AND content <> ''
		  ORDER BY ordinal`, sessionID)
	if err != nil {
		return quality.PromptHygiene{}, 0, fmt.Errorf("gather prompts for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	var folder quality.PromptHygieneFolder
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return quality.PromptHygiene{}, 0, fmt.Errorf("scan prompt for session %d: %w", sessionID, err)
		}
		folder.Add(c)
	}
	if err := rows.Err(); err != nil {
		return quality.PromptHygiene{}, 0, fmt.Errorf("iterate prompts for session %d: %w", sessionID, err)
	}

	var duplicates int
	if err := tx.QueryRow(ctx,
		`WITH p AS (
		   SELECT btrim(regexp_replace(lower(content), '\s+', ' ', 'g')) AS norm
		     FROM messages
		    WHERE session_id = $1 AND role = 'user' AND content <> ''
		      AND array_length(regexp_split_to_array(btrim(content), '\s+'), 1) >= $2
		 )
		 SELECT count(*) - count(DISTINCT norm) FROM p`,
		sessionID, quality.DuplicateMinWords).Scan(&duplicates); err != nil {
		return quality.PromptHygiene{}, 0, fmt.Errorf("count duplicate prompts for session %d: %w", sessionID, err)
	}
	return folder.Result(duplicates), folder.Count(), nil
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
		    peak_context_tokens, context_reset_count,
		    refreshed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18, now())
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
		   refreshed_at = now()`,
		sessionID, quality.Version, string(outcome), string(conf), scoreArg, gradeArg,
		f.toolCalls, f.toolFailures, f.toolRetries, f.editChurn, f.longestFailureStreak,
		f.promptCount, f.hygiene.Short, f.hygiene.Duplicate, f.hygiene.NoCodeContext, f.hygiene.UnstructuredStart,
		f.peakContextTokens, f.contextResets)
	if err != nil {
		return fmt.Errorf("upsert signals for session %d: %w", sessionID, err)
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

// RefreshSettledSignals recomputes signals for every settled session whose stored row is
// missing, stamped at an older signals version, or older than its source. It is the
// production path that materializes signals: the append path no longer refreshes on
// catch-up (see AdvanceProjection), so a session's signals are computed once here, after it
// has been idle past the abandoned threshold, off the ingest hot path.
//
// A session is due when it is settled (ended_at at least abandonedIdleMinutes in the past)
// AND its signals are not already current for that settled state. Three clauses catch every
// way the stored row can disagree with a fresh recompute, so the derived row stays equal to
// its source:
//
//   - No row, or a stale signals_version: nothing current exists, so compute or re-stamp it.
//   - Refreshed before the settle point (refreshed_at earlier than ended_at plus the idle
//     window): a reparse that graded the session while it was still live left an outcome that
//     was not yet stable (abandoned versus unknown turns on the idle gap). The idle window
//     only grows from here, so recomputing once past the settle point pins the outcome.
//   - Refreshed before the projection last changed (refreshed_at earlier than updated_at):
//     the source grew after the row was computed. This is the one ended_at cannot catch,
//     because ended_at is the transcript's own last-activity time, not the ingest time: a
//     historical session uploaded in several chunks keeps an ended_at far in the past, so a
//     later chunk does not move ended_at anywhere near now. applyAggregates stamps updated_at
//     = now() on every appended region (and every reparse), so comparing it to refreshed_at
//     catches a partial-projection grade that a late chunk would otherwise strand. Because
//     both are the transaction clock, a reparse (which refreshes in the same transaction that
//     bumps updated_at) reads them equal and does not re-trigger itself.
//
// It drains the whole due backlog in bounded batches, walking the settled tail once in
// (ended_at, id) order via a keyset cursor: each batch resumes strictly after the last row
// of the previous one, so a row it just refreshed is stepped over rather than rescanned.
// Draining D due sessions is then one forward pass over the settled sessions, O(D_settled),
// not a scan restarted per batch (which would reconsider the already-refreshed prefix every
// time, O(D^2/batch)). Each session is refreshed in its own transaction so one slow session
// never holds a broad lock, cancellation stops the drain between sessions, and it returns
// how many it refreshed.
func (s *Store) RefreshSettledSignals(ctx context.Context) (int, error) {
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

// dueSettledBatch returns up to limit due settled session ids strictly after the
// (afterEnded, afterID) keyset cursor, in (ended_at, id) order, with the cursor to resume
// from (the last row's ended_at and id). "Due" is the four-case predicate documented on
// RefreshSettledSignals. The ids are read up front and the query's connection is released
// (deferred Close) before the caller refreshes them, so the scan does not contend with the
// per-session refresh transactions.
func (s *Store) dueSettledBatch(ctx context.Context, afterEnded time.Time, afterID int64, limit int) ([]int64, time.Time, int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT s.id, s.ended_at
		   FROM sessions s
		   LEFT JOIN session_signals sig ON sig.session_id = s.id
		  WHERE s.ended_at IS NOT NULL
		    AND s.ended_at < now() - make_interval(mins => $1)
		    AND (s.ended_at, s.id) > ($4, $5)
		    AND ( sig.session_id IS NULL
		       OR sig.signals_version <> $2
		       OR sig.refreshed_at < s.ended_at + make_interval(mins => $1)
		       OR sig.refreshed_at < s.updated_at )
		  ORDER BY s.ended_at, s.id
		  LIMIT $3`,
		abandonedIdleMinutes, quality.Version, limit, afterEnded, afterID)
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

// SessionSignalsByID reads a session's current-version stored signals. A session with no
// current-version row (one ingested before signals existed and not yet reparsed, still
// mid-first-parse, or carrying only a stale-version row a running reparse has not yet
// rewritten) reads as an unknown, unscored result rather than an error, so the session
// page renders a neutral state instead of a stale grade. The signals_version filter keeps
// this read in step with the Insights aggregates, which count only current-version rows: a
// session never shows a graded header while the fleet view treats it as unscored.
func (s *Store) SessionSignalsByID(ctx context.Context, sessionID int64) (SessionSignals, error) {
	var sig SessionSignals
	err := s.Pool.QueryRow(ctx,
		`SELECT session_id, signals_version, outcome, outcome_confidence, score, grade,
		        tool_calls, tool_failures, tool_retries, edit_churn, longest_failure_streak,
		        prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start,
		        peak_context_tokens, context_reset_count
		   FROM session_signals WHERE session_id = $1 AND signals_version = $2`, sessionID, quality.Version).Scan(
		&sig.SessionID, &sig.Version, &sig.Outcome, &sig.OutcomeConfidence, &sig.Score, &sig.Grade,
		&sig.ToolCalls, &sig.ToolFailures, &sig.ToolRetries, &sig.EditChurn, &sig.LongestFailureStreak,
		&sig.PromptCount, &sig.ShortPromptCount, &sig.DuplicatePromptCount, &sig.NoCodeContextCount, &sig.UnstructuredStart,
		&sig.PeakContextTokens, &sig.ContextResetCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionSignals{SessionID: sessionID, Outcome: string(quality.OutcomeUnknown), OutcomeConfidence: string(quality.ConfLow)}, nil
	}
	if err != nil {
		return SessionSignals{}, fmt.Errorf("read signals for session %d: %w", sessionID, err)
	}
	return sig, nil
}
