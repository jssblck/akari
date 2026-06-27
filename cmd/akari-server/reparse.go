package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// runReparse rebuilds the parsed projection for stored sessions from their raw
// bytes. This is how a parser improvement reaches already-ingested data without
// re-uploading anything.
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

	targets, err := st.SessionsForReparse(ctx, *agent)
	if err != nil {
		return err
	}

	var ok, failed int
	for _, t := range targets {
		if _, err := parse.SessionFromRaw(ctx, st, t.ID, t.Agent); err != nil {
			failed++
			log.Printf("reparse session %d (%s): %v", t.ID, t.Agent, err)
			continue
		}
		ok++
	}
	fmt.Printf("reparsed %d session(s), %d failed\n", ok, failed)
	return nil
}
