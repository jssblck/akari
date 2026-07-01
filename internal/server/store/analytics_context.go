package store

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/quality"
)

// ContextHealthStats is the cohort's context-load picture over a scope: how heavy the
// scoped sessions' contexts got and how often they shed that context. The peak
// percentiles read straight off the stored per-session peaks (quality.ContextHealth,
// refreshed on catch-up or reparse); the reset figures sum the inferred compaction/clear
// counts. The cohort is the scoped sessions carrying a current-version signals row whose
// peak is measured (a session with no main-thread usage stores NULL and is left out), so
// every rate divides by the same measured set. A stale or missing signals row contributes
// nothing, the same way the quality distribution folds it into unknown, so the panel
// never mixes a half-rebuilt view.
type ContextHealthStats struct {
	Sessions          int   // scoped sessions with a measured peak, the rate denominator
	PeakTokensP50     int64 // median session peak context, in tokens
	PeakTokensP90     int64 // 90th-percentile session peak context
	PeakTokensMax     int64 // heaviest single session peak
	TotalResets       int64 // inferred context resets summed over the cohort
	SessionsWithReset int   // sessions that reset context at least once
}

// HasData reports whether the scope carried any measured session, so the panel can show a
// note rather than a row of zeroes for a window with no context-measured sessions.
func (h ContextHealthStats) HasData() bool { return h.Sessions > 0 }

// ContextHealth aggregates the scoped sessions' context-load figures for the Insights
// page. It shares the analytics filter (clauseFor on s.started_at, so a windowed view
// counts sessions that started in the window) and INNER joins the current-version signals
// row with a non-null peak, so the percentiles and sums cover exactly the sessions whose
// context has been measured at the running model. percentile_disc returns an actual
// stored peak (a real session's token count, not an interpolated value), which is the
// honest thing to report for a "how heavy did a typical session get" figure.
func (s *Store) ContextHealth(ctx context.Context, f AnalyticsFilter) (ContextHealthStats, error) {
	filter, args := f.clauseFor("s.started_at")
	args = append(args, quality.Version)
	var h ContextHealthStats
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*),
		        coalesce(percentile_disc(0.5) WITHIN GROUP (ORDER BY sig.peak_context_tokens), 0),
		        coalesce(percentile_disc(0.9) WITHIN GROUP (ORDER BY sig.peak_context_tokens), 0),
		        coalesce(max(sig.peak_context_tokens), 0),
		        coalesce(sum(sig.context_reset_count), 0),
		        coalesce(sum(CASE WHEN sig.context_reset_count > 0 THEN 1 ELSE 0 END), 0)
		   FROM sessions s
		   JOIN session_signals sig
		     ON sig.session_id = s.id
		    AND sig.signals_version = $`+fmt.Sprint(len(args))+`
		    AND sig.peak_context_tokens IS NOT NULL
		  WHERE TRUE`+filter,
		args...).Scan(&h.Sessions, &h.PeakTokensP50, &h.PeakTokensP90, &h.PeakTokensMax,
		&h.TotalResets, &h.SessionsWithReset)
	if err != nil {
		return ContextHealthStats{}, fmt.Errorf("context health: %w", err)
	}
	return h, nil
}
