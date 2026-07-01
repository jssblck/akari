package store

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/quality"
)

// PromptHygiene is the cohort's input-quality picture over a scope: how many of the
// window's human prompts were terse, repeated, or asked for a change without pointing at
// code, and how many sessions opened with an unstructured prompt. The counts come from
// the stored per-session signals (aggregated by the settle pass or a reparse from the
// per-message hygiene columns quality.ClassifyPrompt materializes at insert), summed
// over the sessions carrying a current-version row so the numerators and the Prompts
// denominator cover the same set. A stale or missing signals row contributes nothing to
// either, the same way the quality distribution folds it into the unknown bucket, so the
// panel never mixes a half-rebuilt view.
type PromptHygiene struct {
	Prompts            int // human prompts across the scoped sessions, the rate denominator
	Short              int // prompts under the terse-word threshold
	Duplicate          int // prompts repeating an earlier one verbatim
	NoCodeContext      int // change requests that pointed at no code
	Sessions           int // scoped sessions carrying a current-version signals row
	UnstructuredStarts int // of those sessions, how many opened with an unstructured prompt
}

// HasData reports whether the scope carried any measured prompt, so the panel can show a
// note rather than a row of zero-over-zero rates for a window with no signalled sessions.
func (h PromptHygiene) HasData() bool { return h.Sessions > 0 && h.Prompts > 0 }

// PromptHygiene aggregates the scoped sessions' prompt-hygiene counts on its own pooled
// connection. Insights instead threads its snapshot transaction through promptHygieneFrom, so
// the measured cohort shares one MVCC snapshot with the other panels.
func (s *Store) PromptHygiene(ctx context.Context, f AnalyticsFilter) (PromptHygiene, error) {
	return s.promptHygieneFrom(ctx, s.Pool, f)
}

// promptHygieneFrom aggregates the scoped sessions' prompt-hygiene counts for the Insights page
// from one querier. It shares the analytics filter (clauseFor on s.started_at, so a windowed
// view counts sessions that started in the window) and INNER joins the current-version signals
// row, so the sums cover exactly the sessions whose hygiene has been measured at the running
// model. The join also requires the row to be usable (NOT s.signals_stale), so a session that
// gained an appended region after its last grade, or was graded while still live, drops out of
// the measured cohort until the settle pass re-grades it, rather than carrying counts computed
// over an earlier or not-yet-settled state. Gating on the flag rather than a
// refreshed_at >= updated_at comparison is deliberate: updated_at also moves on metadata-only
// writes that leave the grade valid, and the flag is set at exactly the projection-change sites.
// That measured cohort (Sessions) is the base for the unstructured-start rate; it can be smaller
// than the page's total session count while a backfill reparse or a re-grade is pending, and the
// parsed views are gated during a reparse, so a reader never sees the two disagree. The prompt
// denominator is the stored prompt_count (the classifier's own base of non-empty prompts), not
// user_message_count, so a numerator can never exceed it and every rate stays within [0, 1].
func (s *Store) promptHygieneFrom(ctx context.Context, q querier, f AnalyticsFilter) (PromptHygiene, error) {
	filter, args := f.clauseFor("s.started_at")
	args = append(args, quality.Version)
	var h PromptHygiene
	err := q.QueryRow(ctx,
		`SELECT coalesce(sum(sig.prompt_count), 0),
		        coalesce(sum(sig.short_prompt_count), 0),
		        coalesce(sum(sig.duplicate_prompt_count), 0),
		        coalesce(sum(sig.no_code_context_count), 0),
		        count(*),
		        coalesce(sum(CASE WHEN sig.unstructured_start THEN 1 ELSE 0 END), 0)
		   FROM sessions s
		   JOIN session_signals sig
		     ON sig.session_id = s.id AND sig.signals_version = $`+fmt.Sprint(len(args))+` AND NOT s.signals_stale
		  WHERE TRUE`+filter,
		args...).Scan(&h.Prompts, &h.Short, &h.Duplicate, &h.NoCodeContext, &h.Sessions, &h.UnstructuredStarts)
	if err != nil {
		return PromptHygiene{}, fmt.Errorf("prompt hygiene: %w", err)
	}
	return h, nil
}
