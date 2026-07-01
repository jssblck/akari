package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// settleBatchSize bounds one RefreshSettledSignals call so a single pass never holds a
// broad lock or a long transaction. A pass drains the backlog by looping in batches, so the
// bound only caps per-call work, not per-pass work.
const settleBatchSize = 500

// runSignalsSettle computes per-session signals for settled sessions on a fixed interval
// until the context is cancelled. The ingest append path deliberately does not recompute
// signals (that would be quadratic in the session's turns, and would grade a still-running
// session with a time-dependent outcome), so this is where a settled session's grade is
// filled in, once, after it has been idle past the abandoned threshold. Each wake drains
// the whole due backlog in bounded batches, so a version bump or a fresh import catches up
// over one pass rather than one session per tick. The pass is bounded by its own timeout so
// a slow catch-up cannot stack up behind the ticker.
func runSignalsSettle(ctx context.Context, st *store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			n, err := drainSettledSignals(passCtx, st)
			cancel()
			switch {
			case err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
				log.Printf("signals settle: %v", err)
			case n > 0:
				log.Printf("signals settle: refreshed %d session(s)", n)
			}
		}
	}
}

// drainSettledSignals refreshes every currently-due session in bounded batches, returning
// the total refreshed. It loops until a batch comes back short, which means the query found
// fewer than a full batch of due sessions and the backlog is drained: each refreshed row
// updates refreshed_at and drops out of the due set, so the next batch is strictly the
// remaining work and the loop makes progress every iteration.
func drainSettledSignals(ctx context.Context, st *store.Store) (int, error) {
	total := 0
	for {
		n, err := st.RefreshSettledSignals(ctx, settleBatchSize)
		total += n
		if err != nil {
			return total, err
		}
		if n < settleBatchSize {
			return total, nil
		}
	}
}

// runSettle computes per-session signals for every settled session that is due, then exits.
// It is the one-shot form of the background pass, for an operator who runs the settle loop
// disabled (AKARI_SIGNALS_SETTLE_INTERVAL=0) and drives it on their own schedule, or who
// wants to force the corpus current after a signals-version bump without waiting for the
// next tick.
func runSettle(args []string) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	migrateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := st.Migrate(migrateCtx, migrations.FS); err != nil {
		return err
	}

	refreshed, err := drainSettledSignals(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("refreshed signals for %d settled session(s)\n", refreshed)
	return nil
}
