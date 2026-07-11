package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

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
func runWatch(ctx context.Context, args []string) (runErr error) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: platform config dir)")
	daemonLogPath := fs.String("daemon-log", "", "internal: log file path used when watch is relaunched as a detached daemon process")
	if err := fs.Parse(args); err != nil {
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

	logf := log.Printf
	if *daemonLogPath != "" {
		output, err := daemon.OpenLog(*daemonLogPath)
		if err != nil {
			return err
		}
		logger := log.New(output, "", log.LstdFlags)
		logf = logger.Printf
		defer func() {
			if runErr != nil {
				if _, err := fmt.Fprintf(output, "akari: %v\n", runErr); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("write daemon startup error: %w", err))
				}
			}
			if err := output.Close(); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}()
	}

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	machine := config.ResolveMachine(cfg, os.Getenv, os.Hostname)

	roots := discover.Roots(cfg, os.Getenv, home)
	resolver := resolve.New()
	client := upload.New(upload.NewHTTPClient(), cfg.ServerURL, cfg.Token)
	// watch is a long-lived host: its idle ticks flush a Codex trailing turn once the
	// settle window elapses, so it never finalizes eagerly.
	sync := syncer.New(resolver, client, machine, false)

	w := watch.New(roots, sync.SyncOne, watch.Options{Excludes: cfg.Excludes, Logf: logf})
	logf("akari watch: watching %d root(s); press Ctrl-C to stop", len(roots))

	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logf("akari watch: stopped")
	return nil
}
