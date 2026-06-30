package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jssblck/akari/internal/client/daemon"
	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/syncer"
	"github.com/jssblck/akari/internal/client/upload"
	"github.com/jssblck/akari/internal/client/watch"
	"github.com/jssblck/akari/internal/config"
)

// runWatch runs the foreground watch loop until interrupted. It holds the
// single-instance lock for its lifetime.
func runWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: platform config dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}

	paths, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}
	lock, err := daemon.Acquire(paths.Pidfile)
	if err != nil {
		return err
	}
	defer lock.Release()

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	machine, _ := os.Hostname()

	roots := discover.Roots(cfg, os.Getenv, home)
	resolver := resolve.New()
	client := upload.New(&http.Client{Timeout: 60 * time.Second}, cfg.ServerURL, cfg.Token)
	sync := syncer.New(resolver, client, machine)

	w := watch.New(roots, sync.SyncOne, watch.Options{Excludes: cfg.Excludes, Logf: log.Printf})
	log.Printf("akari watch: watching %d root(s); press Ctrl-C to stop", len(roots))

	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Printf("akari watch: stopped")
	return nil
}
