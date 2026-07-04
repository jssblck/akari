package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// runReparse rebuilds the parsed projection for stored sessions from their raw
// bytes. This is how a parser improvement reaches already-ingested data without
// re-uploading anything. The running server drains the same rebuild path on its
// own (a bumped parse.Epoch marks every session due at startup); the CLI stays
// as a manual escape hatch that forces the scope due regardless of the epoch
// and drains it in the foreground. Any orphaned blobs the rewrite leaves are
// swept here too, since a CLI run may target a database with no server (and so
// no background sweep) attached.
func runReparse(args []string) error {
	fs := flag.NewFlagSet("reparse", flag.ContinueOnError)
	agent := fs.String("agent", "", "limit to one agent (claude|codex|pi); empty means all")
	if err := fs.Parse(args); err != nil {
		return err
	}

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

	marked, err := st.MarkEpochStale(ctx, *agent)
	if err != nil {
		return err
	}
	fmt.Printf("marked %d session(s) due\n", marked)

	res := parse.NewWorker(st, cfg.ParseWorkers, 0).Drain(ctx)
	swept, err := st.SweepBlobs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("reparsed %d session(s), %d failed; swept %d orphaned blob(s)\n",
		res.Done-res.Failed, res.Failed, swept)
	return nil
}
