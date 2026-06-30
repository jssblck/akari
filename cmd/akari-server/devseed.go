package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/devseed"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/shutdown"
	"github.com/jssblck/akari/migrations"
)

// runDevSeed fills a local server with example data for development. It is meant
// to run as the eph [server] service's post-start hook (where AKARI_DATABASE_URL
// and AKARI_URL are already in the environment), but works standalone too.
//
// By default it is best-effort: any failure is logged and the command still exits
// 0, so wiring it into post-start never blocks `eph up`. Pass --strict to surface
// failures with a non-zero exit when running it by hand.
func runDevSeed(args []string) error {
	fs := flag.NewFlagSet("dev-seed", flag.ContinueOnError)
	users := fs.Int("users", 4, "number of demo accounts to create (clamped to the built-in roster)")
	password := fs.String("password", "akari-dev", "shared login password for every demo account")
	serverURL := fs.String("server-url", "", "base URL of the running server to upload through (default: $AKARI_URL, else derived from AKARI_LISTEN)")
	timeLimit := fs.Duration("time-limit", 30*time.Second, "how long to keep starting new uploads while ingesting local sessions (0 for no limit)")
	concurrency := fs.Int("concurrency", 8, "max session files to upload in parallel")
	force := fs.Bool("force", false, "re-seed even if the store already holds sessions: clears existing sessions first, then re-ingests and re-shuffles")
	strict := fs.Bool("strict", false, "exit non-zero on failure (default: best-effort, exit 0 so an eph post-start hook never blocks `eph up`)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadServer()
	if err != nil {
		return finishDevSeed(err, *strict)
	}

	url := *serverURL
	if url == "" {
		url = os.Getenv("AKARI_URL")
	}
	if url == "" {
		url = deriveServerURL(cfg.Listen)
	}

	// The first Ctrl-C cancels ctx so an in-flight ingest winds down gracefully,
	// mirroring the time limit; a second exits at once.
	ctx, stop := shutdown.Notify(func() {
		log.Printf("dev-seed: interrupt received, winding down")
	})
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return finishDevSeed(err, *strict)
	}
	defer st.Close()

	migrateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := st.Migrate(migrateCtx, migrations.FS); err != nil {
		return finishDevSeed(err, *strict)
	}

	err = devseed.Run(ctx, st, devseed.Options{
		ServerURL:   url,
		NumUsers:    *users,
		Password:    *password,
		TimeLimit:   *timeLimit,
		Concurrency: *concurrency,
		Force:       *force,
	})
	return finishDevSeed(err, *strict)
}

// finishDevSeed downgrades a failure to a logged warning unless strict is set, so
// the default post-start use never aborts `eph up` on a transient hiccup.
func finishDevSeed(err error, strict bool) error {
	if err == nil {
		return nil
	}
	if strict {
		return err
	}
	log.Printf("dev-seed: %v (continuing; pass --strict to fail)", err)
	return nil
}

// deriveServerURL turns a listen address like ":8080" or "0.0.0.0:8080" into a
// loopback base URL the in-process client can reach. It is only a fallback for
// when neither --server-url nor AKARI_URL is set.
func deriveServerURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://localhost:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, port))
}
