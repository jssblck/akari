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

// runSettleMaintenance runs the two settled-corpus maintenance drains on a fixed interval
// until the context is cancelled: the per-session signals refresh and the cache-savings
// backfill. The ingest append path deliberately does not recompute signals (that would be
// quadratic in the session's turns, and would grade a still-running session with a
// time-dependent outcome), so this is where a settled session's grade is filled in, once,
// after it has been idle past the abandoned threshold. Each wake drains the whole due
// backlog (RefreshSettledSignals keyset-walks the settled tail once), so a version bump or a
// fresh import catches up in one pass rather than one session per tick. Each pass is bounded
// by its own timeout so a slow catch-up cannot stack up behind the ticker.
//
// The cache-savings backfill runs on the SAME tick, not just once at startup, because a
// pricing rolling deploy keeps minting candidates after any one-shot drain finishes: while an
// old binary (pricing.Version N-1) and a new one (N) share the database, the old binary's
// applyAggregates drops cache_savings_backfilled=false on every cache-bearing append it folds
// at the superseded rates (the marker is ahead of it), and those appends land at session ids
// the new binary's startup drain has already walked past or after it completed. A one-shot
// drain would leave those candidates flagged provisional until an unrelated read repriced them,
// so a session no one opened would sit with an old-rate rollup in the fleet totals. Draining
// each tick guarantees the newer binary consumes every candidate the reconcile marker made,
// bounded and idempotent: with nothing due the reconcile is one O(1) marker read and the drain's
// first batch is empty.
func runSettleMaintenance(ctx context.Context, st *store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			n, err := st.RefreshSettledSignals(passCtx)
			cancel()
			switch {
			case errors.Is(err, context.Canceled):
				// Shutdown cancelled the pass; the loop returns on the next ctx.Done check.
			case errors.Is(err, context.DeadlineExceeded):
				// The pass hit its own 5m timeout before draining the due backlog, which a large or
				// slow corpus can cause, so some settled sessions are still ungraded. Unlike shutdown
				// this is worth surfacing: the next tick resumes the keyset walk, but an operator
				// seeing it repeatedly should raise the interval or run `akari-server settle`.
				log.Printf("signals settle: pass timed out before draining the due backlog; it resumes next tick")
			case err != nil:
				log.Printf("signals settle: %v", err)
			case n > 0:
				log.Printf("signals settle: refreshed %d session(s)", n)
			}

			// A separate timeout so a slow signals pass cannot starve the cache-savings drain of its
			// budget, and vice versa. The drain restarts its keyset walk from the start each tick, so a
			// tick that times out mid-walk simply resumes next tick; candidates only shrink as they price.
			bpCtx, bcancel := context.WithTimeout(ctx, 5*time.Minute)
			bn, berr := st.BackfillCacheSavings(bpCtx)
			bcancel()
			switch {
			case errors.Is(berr, context.Canceled):
				// Shutdown cancelled the drain; the loop returns on the next ctx.Done check.
			case errors.Is(berr, context.DeadlineExceeded):
				log.Printf("cache-savings backfill: drain timed out before consuming every candidate; it resumes next tick")
			case berr != nil:
				log.Printf("cache-savings backfill: %v", berr)
			case bn > 0:
				log.Printf("cache-savings backfill: priced %d session(s)", bn)
			}
		}
	}
}

// runSettle runs both settled-corpus maintenance drains once, then exits. It is the one-shot
// form of the background loop, for an operator who runs the loop disabled
// (AKARI_SIGNALS_SETTLE_INTERVAL=0) and drives it on their own schedule, or who wants to force
// the corpus current after a signals- or pricing-version bump without waiting for the next tick.
// It mirrors the loop's two drains so the disabled-loop mode is not left without the periodic
// cache-savings reprice.
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

	refreshed, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("refreshed signals for %d settled session(s)\n", refreshed)

	priced, err := st.BackfillCacheSavings(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("priced cache savings for %d session(s)\n", priced)
	return nil
}
