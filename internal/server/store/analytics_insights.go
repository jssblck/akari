package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Insights is everything the Insights page renders for a scope: the quality
// distribution (grades and outcomes), the archetype mix, the concurrency figures, the
// velocity cadence, the tool reliability and mix, the prompt-hygiene rates, the file
// churn, and the context-load figures. It is the cross-cutting counterpart to Analytics
// (which is about cost and tokens), scoped by the same AnalyticsFilter so a window or a
// per-user narrowing applies to both surfaces alike.
type Insights struct {
	Quality     QualityDistribution
	Archetypes  []LabeledCount
	Concurrency ConcurrencyStats
	Velocity    VelocityStats
	Tools       ToolStats
	Hygiene     PromptHygiene
	Churn       FileChurn
	Context     ContextHealthStats
	// Users is the per-author quality leaderboard: who ran the window's sessions and how
	// their work graded. It shares the snapshot so its per-user session counts reconcile
	// with the quality total the distributions read.
	Users UserQualityStats
}

// HasData reports whether any scoped session carried signals, so the page can show an
// empty state instead of a row of zero bars on a scope with no sessions.
func (i Insights) HasData() bool { return i.Quality.Sessions > 0 }

// Insights gathers the page's panels in one repeatable-read snapshot. The panels overlap: the
// quality split's session total, the archetype split, and the cohort denominators of the
// hygiene, context, and tool panels all describe the same scoped session set, so reading them
// on separate pooled connections would let a concurrent ingest land between two panels and make
// the page disagree with itself (the quality total and the archetype total, say, off by the one
// session that arrived mid-render). One read-only REPEATABLE READ transaction pins every panel
// to the same MVCC snapshot, so the overlapping totals reconcile exactly.
//
// Unlike AnalyticsSnapshot it takes no reparse-lock gate: a live page tolerates a snapshot that
// falls during a reparse (each session is atomically old or new in it, since ReparseSession
// commits per session), it just must not straddle two snapshots within one render. The panels
// still fail fast on the first error, and the read-only transaction takes no row locks, so it
// never blocks ingest.
func (s *Store) Insights(ctx context.Context, f AnalyticsFilter) (Insights, error) {
	var out Insights
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			if out.Quality, err = s.qualityDistributionFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Archetypes, err = s.archetypeDistributionFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Concurrency, err = s.concurrencyStatsFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Velocity, err = s.velocityStatsFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Tools, err = s.toolStatsFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Hygiene, err = s.promptHygieneFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Churn, err = s.fileChurnFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Context, err = s.contextHealthFrom(ctx, tx, f); err != nil {
				return err
			}
			if out.Users, err = s.userQualityFrom(ctx, tx, f); err != nil {
				return err
			}
			return nil
		})
	if err != nil {
		// The panel queries name their own failures, but a begin or commit failure on the
		// snapshot transaction would otherwise surface as a raw database error; name it as an
		// insights-snapshot failure so the /insights 500 path is diagnosable.
		return Insights{}, fmt.Errorf("insights snapshot: %w", err)
	}
	return out, nil
}
