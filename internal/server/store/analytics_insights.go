package store

import (
	"context"
	"fmt"
	"time"

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

	// Trends is the time-bucketed read of the same cohort: every distribution above drawn
	// as a series over the window's day or week buckets. It is computed only when the
	// filter names a bucket (AnalyticsFilter.Bucket), so a distributions-only caller pays
	// nothing for it and leaves it nil. It shares the snapshot transaction, so a bucket
	// series reconciles exactly with the rolled-up number above it.
	Trends *Trends
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
			// When the caller wants the trend grid, bound the whole page to the charted
			// span first. The grid caps an unbounded "all" window at maxTrendBuckets, so
			// pinning f.Since to the first rendered bucket makes the headline distributions
			// and every trend total count the same sessions, and keeps each panel's query
			// from scanning history the charts will not show. A bounded range already starts
			// at or after its grid's first bucket, so its window is left unchanged (the grid
			// start only moves f.Since forward, never back).
			if f.Bucket != "" {
				now := time.Now()
				since, terr := s.resolveTrendSince(ctx, tx, f, now)
				if terr != nil {
					return terr
				}
				if g := newTrendGrid(f.Bucket, since, now); g.n() > 0 && g.Starts[0].After(f.Since) {
					f.Since = g.Starts[0]
				}
			}
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
			// The per-author leaderboard is skipped when the caller will not render it
			// (OmitUsers, set by the public project overview, whose quality band carries no
			// People panel), so the read does not group every session by user and build an
			// aggregate proportional to the scope's user count only to discard it. It sits
			// outside every other panel's totals, so leaving Users zero changes nothing else.
			if !f.OmitUsers {
				if out.Users, err = s.userQualityFrom(ctx, tx, f); err != nil {
					return err
				}
			}
			// The trend grid is computed only when the caller names a bucket. It reuses the
			// context stats already read above for the peak-context histogram markers, so the
			// markers on the trend annotate the same cohort the context panel summarizes.
			if f.Bucket != "" {
				if out.Trends, err = s.trendsFrom(ctx, tx, f, out.Context); err != nil {
					return err
				}
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
