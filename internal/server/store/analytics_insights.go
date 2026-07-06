package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
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

// insightsPanelLimit bounds how many panels read concurrently, so one Insights call
// never drains the pool out from under other requests. The control connection holds one
// slot for the whole call, so peak usage is this many plus one; kept a few below a
// default pool's ceiling to leave headroom for concurrent traffic. Insights further caps
// the worker count at the connections actually spare beyond the control one, so a pool
// smaller than this constant cannot deadlock (see panelWorkers).
const insightsPanelLimit = 6

// panelWorkers is how many panels Insights may read concurrently given the pool's connection
// ceiling. Each concurrent panel acquires its own connection while the control transaction
// holds one open until Wait, so the safe worker count is the connections spare beyond that
// control one, capped at insightsPanelLimit. It returns 0 when nothing is spare (a pool sized
// at or near one connection, as some constrained deployments configure): the caller must then
// run the panels sequentially on the control transaction, because a concurrent panel would
// block forever in Acquire waiting on the very connection this call is holding. That zero is
// the guard against a self-inflicted deadlock, not a performance knob.
func panelWorkers(maxConns int) int {
	spare := maxConns - 1
	if spare < 1 {
		return 0
	}
	return min(insightsPanelLimit, spare)
}

// snapshotIDRe validates a pg_export_snapshot token before it is interpolated into SET
// TRANSACTION SNAPSHOT (which takes no bind parameters). The token is server-generated,
// never user input, so this is defence in depth rather than a real injection boundary:
// it fails loudly if a future Postgres changes the token shape rather than splicing an
// unexpected string into SQL.
var snapshotIDRe = regexp.MustCompile(`^[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9A-Fa-f]+$`)

// Insights gathers the page's panels concurrently against one shared MVCC snapshot. The
// panels overlap: the quality split's session total, the archetype split, and the cohort
// denominators of the hygiene, context, and tool panels all describe the same scoped
// session set, so reading them on independent connections that each took their own snapshot
// would let a concurrent ingest land between two panels and make the page disagree with
// itself (the quality total and the archetype total, say, off by the one session that
// arrived mid-render).
//
// A control transaction fixes the snapshot and exports it (pg_export_snapshot); every panel
// then runs on its own pooled connection under a read-only REPEATABLE READ transaction that
// imports that exact snapshot (SET TRANSACTION SNAPSHOT). So the overlapping totals still
// reconcile exactly, as they did under the old single-transaction sequential read, but the
// panels' wall time is now the slowest single panel rather than their sum: the roughly
// eighteen aggregate queries the page runs no longer serialize on one connection. The
// control transaction stays open until every panel has imported the snapshot (it is the
// last thing released), which is what keeps the exported snapshot valid for the importers.
//
// Unlike AnalyticsSnapshot it takes no reparse-lock gate: a live page tolerates a snapshot
// that falls during a reparse (each session is atomically old or new in it, since
// ReparseSession commits per session), it just must not straddle two snapshots within one
// render. The panels fail fast on the first error (the errgroup cancels the rest), and every
// transaction is read-only and takes no row locks, so the call never blocks ingest.
func (s *Store) Insights(ctx context.Context, f AnalyticsFilter) (Insights, error) {
	var out Insights

	ctrl, err := s.Pool.Acquire(ctx)
	if err != nil {
		return Insights{}, fmt.Errorf("insights snapshot: acquire control conn: %w", err)
	}
	defer ctrl.Release()
	ctrlTx, err := ctrl.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Insights{}, fmt.Errorf("insights snapshot: begin control tx: %w", err)
	}
	// Read-only: rollback is the clean release and never loses work.
	defer ctrlTx.Rollback(ctx)

	// When the caller wants the trend grid, bound the whole page to the charted span first,
	// on both edges. The grid caps an unbounded "all" window at maxTrendBuckets and stops at
	// the current bucket, so pinning f.Since to the first rendered bucket and f.Until to the
	// end of the last one makes the headline distributions and every trend total count the
	// same rows the series draw, and keeps each panel's query from scanning history the charts
	// will not show. Without the upper bound a row timestamped ahead of render time counts in
	// the headline while the grid drops its future bucket (g.index < 0), so the two disagree.
	// Both bounds only tighten the window: a bounded range already sits inside its grid, so
	// f.Since never moves back and f.Until never moves forward. Resolving this on the control
	// transaction fixes its snapshot (its first read), which is the snapshot every panel then
	// shares, so the tightened window and the panels describe one consistent cohort.
	if f.Bucket != "" {
		now := time.Now()
		since, terr := s.resolveTrendSince(ctx, ctrlTx, f, now)
		if terr != nil {
			return Insights{}, fmt.Errorf("insights snapshot: %w", terr)
		}
		if g := newTrendGrid(f.Bucket, since, now); g.n() > 0 {
			if g.Starts[0].After(f.Since) {
				f.Since = g.Starts[0]
			}
			if upper := advanceBucket(g.Unit, g.Starts[g.n()-1]); f.Until.IsZero() || upper.Before(f.Until) {
				f.Until = upper
			}
		}
	}

	var snapshotID string
	if err := ctrlTx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshotID); err != nil {
		return Insights{}, fmt.Errorf("insights snapshot: export snapshot: %w", err)
	}
	if !snapshotIDRe.MatchString(snapshotID) {
		return Insights{}, fmt.Errorf("insights snapshot: unexpected snapshot id %q", snapshotID)
	}

	// The panels, each writing a distinct out field. They run either concurrently (each on its
	// own pooled connection importing the control snapshot) or, when the pool cannot spare a
	// connection beyond the one the control transaction holds, sequentially on the control
	// transaction itself. Distinct panels write distinct out fields and both dispatch paths
	// synchronize before out is read, so the writes are race-free either way.
	panels := []func(context.Context, querier) error{
		func(ctx context.Context, q querier) (err error) {
			out.Quality, err = s.qualityDistributionFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Archetypes, err = s.archetypeDistributionFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Concurrency, err = s.concurrencyStatsFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Velocity, err = s.velocityStatsFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Tools, err = s.toolStatsFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Hygiene, err = s.promptHygieneFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Churn, err = s.fileChurnFrom(ctx, q, f)
			return
		},
		// Context and, when charted, the trend grid share one panel: the trend reuses the context
		// stats for its peak-context histogram markers, so it must run after Context, and keeping
		// the pair on one connection avoids threading the intermediate result across goroutines.
		func(ctx context.Context, q querier) (err error) {
			if out.Context, err = s.contextHealthFrom(ctx, q, f); err != nil {
				return err
			}
			if f.Bucket != "" {
				out.Trends, err = s.trendsFrom(ctx, q, f, out.Context)
			}
			return err
		},
	}
	// The per-author leaderboard is skipped when the caller will not render it (OmitUsers, set
	// by the public project overview, whose quality band carries no People panel), so the read
	// does not group every session by user and build an aggregate proportional to the scope's
	// user count only to discard it. It sits outside every other panel's totals, so leaving
	// Users zero changes nothing else.
	if !f.OmitUsers {
		panels = append(panels, func(ctx context.Context, q querier) (err error) {
			out.Users, err = s.userQualityFrom(ctx, q, f)
			return
		})
	}

	// With no connection spare beyond the one the control transaction holds, run the panels in
	// order on the control transaction itself, which already sees the fixed snapshot and needs
	// no further connection. This is what keeps a tiny pool from deadlocking; see panelWorkers.
	workers := panelWorkers(int(s.Pool.Config().MaxConns))
	if workers == 0 {
		for _, p := range panels {
			if err := p(ctx, ctrlTx); err != nil {
				return Insights{}, fmt.Errorf("insights snapshot: %w", err)
			}
		}
		return out, nil
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for _, p := range panels {
		p := p
		g.Go(func() error {
			c, err := s.Pool.Acquire(gctx)
			if err != nil {
				return fmt.Errorf("acquire panel conn: %w", err)
			}
			defer c.Release()
			tx, err := c.BeginTx(gctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				return fmt.Errorf("begin panel tx: %w", err)
			}
			defer tx.Rollback(gctx)
			// SET TRANSACTION SNAPSHOT must precede the transaction's first query; it is the
			// first statement here, so the panel reads the control snapshot.
			if _, err := tx.Exec(gctx, "SET TRANSACTION SNAPSHOT '"+snapshotID+"'"); err != nil {
				return fmt.Errorf("import snapshot: %w", err)
			}
			return p(gctx, tx)
		})
	}
	if err := g.Wait(); err != nil {
		// The panel queries name their own failures; wrap so the /insights 500 path reads as an
		// insights-snapshot failure rather than a raw database error.
		return Insights{}, fmt.Errorf("insights snapshot: %w", err)
	}
	return out, nil
}
