package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/store"
)

// runBackgroundSweep reclaims orphaned CAS blobs on a fixed interval until the
// context is cancelled. Each pass is bounded by its own timeout so a slow sweep
// cannot stack up behind the ticker.
func runBackgroundSweep(ctx context.Context, st *store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			n, err := st.SweepBlobs(sweepCtx)
			cancel()
			switch {
			case err != nil:
				log.Printf("background sweep: %v", err)
			case n > 0:
				log.Printf("background sweep reclaimed %d blob(s)", n)
			}
		}
	}
}

// runSweep deletes CAS blobs no live row references and unlinks their large
// objects. Liveness is computed, not refcounted, so the sweep is safe to run any
// time; it only needs to run after a delete or re-parse, the only events that can
// orphan a blob.
func runSweep(args []string) error {
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

	if err := migrateStore(ctx, st); err != nil {
		return err
	}

	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("swept %d orphaned blob(s)\n", removed)
	return nil
}
