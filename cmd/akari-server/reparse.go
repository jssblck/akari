package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/reparse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// runReparse rebuilds the parsed projection for stored sessions from their raw
// bytes. This is how a parser improvement reaches already-ingested data without
// re-uploading anything. It is now a thin wrapper over the shared reparse service
// (the server runs the same path automatically when the parser epoch changes); the
// CLI stays as a manual escape hatch and forces a run regardless of the epoch.
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

	res, err := reparse.New(ctx, st).Run(ctx, reparse.Options{Agent: *agent, Force: true})
	if err != nil {
		return err
	}
	fmt.Printf("reparsed %d session(s), %d failed; swept %d orphaned blob(s)\n",
		res.Done-res.Failed, res.Failed, res.SweptBlobs)
	return nil
}
