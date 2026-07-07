package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// InsightsPanels names the optional instrument groups one Insights call computes. The
// quality core (the grade/outcome distribution, archetypes, hygiene, context health, and,
// when the filter names a bucket, the signal trend series) is always computed: it is
// cheap (one row per session off sessions and session_signals), and Quality gates
// Insights.HasData, so every surface needs it. Everything else is opt-in, because the
// callers genuinely differ: the fleet /insights page renders all seven instruments, but
// the project page's quality band renders only the signal series plus the Tools
// instrument, and it used to pay for the gallery, velocity, economics, rhythm, and
// subagent scans anyway. A skipped group leaves its Insights (and Trends) fields zero,
// which the JSON payload serializes as empty series no chart mount reads.
type InsightsPanels struct {
	// FleetMix is the per-bucket token share by model (Trends.FleetMix).
	FleetMix bool
	// Gallery is the per-session duration-by-cost scatter and its summary figures
	// (Trends.Gallery).
	Gallery bool
	// Velocity covers the velocity instrument as a whole: the rolled-up cadence figures
	// (Insights.Velocity), the per-bucket series (Trends.Velocity), the hour-of-week
	// rhythm grid (Trends.Rhythm), and the concurrency figures (Insights.Concurrency)
	// the instrument's readouts annotate. These are the message-scan panels, the most
	// expensive reads on the page, so a surface without the instrument skips them all.
	Velocity bool
	// Tools is the tool reliability/mix instrument: Insights.Tools and Trends.Tools.
	Tools bool
	// Churn is the file-churn read the Tools instrument's churn tab draws:
	// Insights.Churn and Trends.Churn.
	Churn bool
	// Economics is the spend-by-outcome and cache-savings series (Trends.Economics).
	Economics bool
	// Subagents is the delegation read (Trends.Subagents).
	Subagents bool
}

// AllInsightsPanels asks for every instrument group, the fleet /insights page's set.
var AllInsightsPanels = InsightsPanels{
	FleetMix: true, Gallery: true, Velocity: true, Tools: true,
	Churn: true, Economics: true, Subagents: true,
}

// QualityBandPanels is the project quality band's set (authed and public): the signal
// series come with the core, and the band renders the Tools instrument beside them.
var QualityBandPanels = InsightsPanels{Tools: true, Churn: true}

// insightsPanelLimit bounds how many panels read concurrently, so one Insights call
// never drains the pool out from under other requests. The control connection holds one
// slot for the whole call, so peak usage is this many plus one; kept a few below a
// default pool's ceiling to leave headroom for concurrent traffic. Insights further caps
// the worker count at the connections actually spare beyond the control one, so a pool
// smaller than this constant cannot deadlock (see panelWorkers).
const insightsPanelLimit = 6

// panelWorkers is the most spare connections one Insights call will borrow to read panels
// concurrently, given the pool's connection ceiling. The control transaction holds one
// connection open until Wait, so the room beyond it is MaxConns-1, capped at insightsPanelLimit
// to leave headroom for other traffic. It returns 0 when nothing is spare (a pool sized at or
// near one connection, as some constrained deployments configure), which routes every panel
// onto the control transaction. This is an upper bound, not a reservation: Insights borrows
// only connections the pool has idle at dispatch time, so it never blocks for one and cannot
// deadlock the pool against itself or other concurrent callers.
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
func (s *Store) Insights(ctx context.Context, f AnalyticsFilter, panels InsightsPanels) (Insights, error) {
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

	// When the caller wants the trend grid, resolve it once on the control transaction and bound
	// the whole page to its charted span, on both edges. The grid caps an unbounded "all" window
	// at maxTrendBuckets and stops at the current bucket, so pinning f.Since to the first rendered
	// bucket and f.Until to the end of the last one makes the headline distributions and every
	// trend total count the same rows the series draw, and keeps each panel's query from scanning
	// history the charts will not show. Without the upper bound a row timestamped ahead of render
	// time counts in the headline while the grid drops its future bucket (g.index < 0), so the two
	// disagree. Both bounds only tighten the window: a bounded range already sits inside its grid,
	// so f.Since never moves back and f.Until never moves forward. Resolving this on the control
	// transaction fixes its snapshot (its first read), which is the snapshot every panel then
	// shares, so the tightened window and the panels describe one consistent cohort.
	//
	// The resolved grid is kept (not thrown away): every per-bucket trend panel below reads it, so
	// the whole grid is computed once here rather than re-resolved inside a trends builder.
	var grid trendGrid
	var wantTrends bool
	if f.Bucket != "" {
		now := time.Now()
		since, terr := s.resolveTrendSince(ctx, ctrlTx, f, now)
		if terr != nil {
			return Insights{}, fmt.Errorf("insights snapshot: %w", terr)
		}
		grid = newTrendGrid(f.Bucket, since, now)
		if grid.n() > 0 {
			if grid.Starts[0].After(f.Since) {
				f.Since = grid.Starts[0]
			}
			if upper := advanceBucket(grid.Unit, grid.Starts[grid.n()-1]); f.Until.IsZero() || upper.Before(f.Until) {
				f.Until = upper
			}
			wantTrends = true
		}
		// Pre-allocate the grid-shaped Trends so the trend panels can each write a distinct field
		// concurrently (the same distinct-field discipline the distribution panels rely on). An
		// empty grid still yields a non-nil Trends whose HasData reports false, matching the old
		// behaviour where a bucketed call with no buckets returned an empty grid rather than nil.
		out.Trends = &Trends{Unit: grid.Unit, BucketStarts: grid.Starts, Labels: grid.labels()}
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
	// The always-on quality core first, then the opt-in instrument groups. A group the
	// caller's panel set leaves false contributes no query at all; its Insights and
	// Trends fields stay zero and the caller's surface has no mount that reads them.
	reads := []func(context.Context, querier) error{
		func(ctx context.Context, q querier) (err error) {
			out.Quality, err = s.qualityDistributionFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Archetypes, err = s.archetypeDistributionFrom(ctx, q, f)
			return
		},
		func(ctx context.Context, q querier) (err error) {
			out.Hygiene, err = s.promptHygieneFrom(ctx, q, f)
			return
		},
		// Context health, and (when charted) the per-bucket signal series with it: the signal
		// trend annotates its peak-context histogram with the context panel's own order
		// statistics (out.Context), so it must run after Context, and keeping the pair on one
		// connection avoids threading that intermediate result across goroutines. Every other
		// trend builder is independent, so they each become their own panel below and fan out
		// across the pool instead of serializing on this one connection.
		func(ctx context.Context, q querier) (err error) {
			if out.Context, err = s.contextHealthFrom(ctx, q, f); err != nil {
				return err
			}
			if wantTrends {
				out.Trends.Signals, err = s.signalTrendsFrom(ctx, q, f, grid, out.Context)
			}
			return err
		},
	}
	if panels.Velocity {
		reads = append(reads,
			func(ctx context.Context, q querier) (err error) {
				out.Concurrency, err = s.concurrencyStatsFrom(ctx, q, f)
				return
			},
			func(ctx context.Context, q querier) (err error) {
				out.Velocity, err = s.velocityStatsFrom(ctx, q, f)
				return
			},
		)
	}
	if panels.Tools {
		reads = append(reads, func(ctx context.Context, q querier) (err error) {
			out.Tools, err = s.toolStatsFrom(ctx, q, f)
			return
		})
	}
	if panels.Churn {
		reads = append(reads, func(ctx context.Context, q querier) (err error) {
			out.Churn, err = s.fileChurnFrom(ctx, q, f)
			return
		})
	}
	// The rest of the trend grid: one panel per independent builder, each writing a distinct
	// out.Trends field. Splitting them out is the whole point of this pass. The grid used to be
	// built by trendsFrom, which ran all of these serially on the single Context panel's
	// connection, so the page's wall time was the sum of every trend query even though seven
	// panel lanes sat idle. As distinct panels they join the existing fan-out (each on a borrowed
	// connection under the shared snapshot), so the trend cost collapses toward the slowest single
	// builder. They still reconcile exactly with the distributions: all of them import the one
	// control snapshot, so they read the same cohort.
	if wantTrends {
		if panels.FleetMix {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.FleetMix, err = s.fleetMixFrom(ctx, q, f, grid)
				return
			})
		}
		if panels.Economics {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.Economics, err = s.economicsFrom(ctx, q, f, grid)
				return
			})
		}
		if panels.Velocity {
			reads = append(reads,
				func(ctx context.Context, q querier) (err error) {
					out.Trends.Velocity, err = s.velocityTrendsFrom(ctx, q, f, grid)
					return
				},
				func(ctx context.Context, q querier) (err error) {
					out.Trends.Rhythm, err = s.rhythmFrom(ctx, q, f)
					return
				},
			)
		}
		if panels.Tools {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.Tools, err = s.toolTrendsFrom(ctx, q, f, grid)
				return
			})
		}
		if panels.Churn {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.Churn, err = s.churnTrendFrom(ctx, q, f, grid)
				return
			})
		}
		if panels.Gallery {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.Gallery, err = s.galleryFrom(ctx, q, f)
				return
			})
		}
		if panels.Subagents {
			reads = append(reads, func(ctx context.Context, q querier) (err error) {
				out.Trends.Subagents, err = s.subagentTrendsFrom(ctx, q, f, grid)
				return
			})
		}
	}

	// Dispatch the panels without ever letting this call hold a connection while blocking to
	// acquire another. That hold-and-wait is the deadlock: N concurrent Insights calls (the
	// project and public-project pages reach this without the HTTP cache) would each pin a
	// control connection, exhaust the pool, then all block acquiring a panel connection that can
	// never free. So the call borrows only the connections the pool has idle right now
	// (AcquireAllIdle never blocks), capped by panelWorkers to leave headroom, and runs the rest
	// of the panels on the control transaction it already holds. Under contention it borrows
	// nothing and the read is fully sequential on the control connection, which is deadlock-free
	// because the call then waits on no further connection: it finishes and releases, so any
	// caller blocked acquiring a control connection makes progress.
	var panelConns []*pgxpool.Conn
	if workers := panelWorkers(int(s.Pool.Config().MaxConns)); workers > 0 {
		idle := s.Pool.AcquireAllIdle(ctx)
		keep := min(len(idle), workers)
		panelConns = idle[:keep]
		for _, c := range idle[keep:] {
			c.Release()
		}
	}
	defer func() {
		for _, c := range panelConns {
			c.Release()
		}
	}()

	// Each executor runs its assigned panels sequentially on one connection: the control
	// transaction, or a borrowed connection under an imported snapshot. Executors run
	// concurrently, so no connection is ever used by two goroutines at once. Round-robin keeps
	// the per-executor panel counts even; with no borrowed connections, every panel lands on the
	// control executor and the read is sequential.
	executors := len(panelConns) + 1
	buckets := make([][]func(context.Context, querier) error, executors)
	for i, p := range reads {
		buckets[i%executors] = append(buckets[i%executors], p)
	}

	g, gctx := errgroup.WithContext(ctx)
	// Executor 0 runs on the control transaction directly (it already holds the snapshot).
	g.Go(func() error {
		for _, p := range buckets[0] {
			if err := p(gctx, ctrlTx); err != nil {
				return err
			}
		}
		return nil
	})
	// The remaining executors each run on a borrowed connection under the imported snapshot.
	for i, c := range panelConns {
		i, c := i, c
		g.Go(func() error {
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
			for _, p := range buckets[i+1] {
				if err := p(gctx, tx); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// The panel queries name their own failures; wrap so the /insights 500 path reads as an
		// insights-snapshot failure rather than a raw database error.
		return Insights{}, fmt.Errorf("insights snapshot: %w", err)
	}
	return out, nil
}
