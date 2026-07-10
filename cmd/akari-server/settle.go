package main

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
)

// runSettle grades every settled-but-ungraded session once, then exits. The
// running server does the same work on the parse worker's maintenance tick;
// this one-shot form exists for an operator who runs with the tick disabled
// (AKARI_SIGNALS_SETTLE_INTERVAL=0) and drives grading on their own schedule,
// or who wants the corpus current right now instead of at the next tick.
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

	if err := migrateStore(ctx, st); err != nil {
		return err
	}
	// The grading guard keys on the running epoch (RefreshSessionSignals skips
	// projections another epoch owns); without this an old settle binary would
	// run unguarded and could clobber a newer binary's work.
	st.SetParserEpoch(parse.Epoch)

	refreshed, err := st.RefreshSettledSignals(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("refreshed signals for %d settled session(s)\n", refreshed)
	return nil
}
