package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

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

	migrateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := st.Migrate(migrateCtx, migrations.FS); err != nil {
		return err
	}

	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("swept %d orphaned blob(s)\n", removed)
	return nil
}
