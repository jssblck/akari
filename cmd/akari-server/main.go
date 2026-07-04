// Command akari-server ingests, stores, parses, and serves agent sessions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	// Embed the IANA timezone database in the binary so time.LoadLocation resolves a
	// viewer's zone (from the tz cookie) even in a scratch container or on Windows,
	// where the system zoneinfo is absent or unreliable. The web UI localizes every
	// stamp against it; without this, LoadLocation would fail and every zone would
	// silently fall back to UTC.
	_ "time/tzdata"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/httpapi"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/shutdown"
	"github.com/jssblck/akari/internal/version"
	"github.com/jssblck/akari/migrations"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version.String())
			return
		case "update":
			if err := runUpdate(os.Args[2:]); err != nil {
				log.Fatalf("akari-server update: %v", err)
			}
			return
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "reparse" {
		if err := runReparse(os.Args[2:]); err != nil {
			log.Fatalf("akari-server reparse: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "sweep" {
		if err := runSweep(os.Args[2:]); err != nil {
			log.Fatalf("akari-server sweep: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "settle" {
		if err := runSettle(os.Args[2:]); err != nil {
			log.Fatalf("akari-server settle: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "dev-seed" {
		if err := runDevSeed(os.Args[2:]); err != nil {
			log.Fatalf("akari-server dev-seed: %v", err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatalf("akari-server: %v", err)
	}
}

func run() error {
	log.Printf("akari-server %s starting", version.String())

	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}

	// rootCtx is cancelled on the first interrupt so background work (the blob
	// sweep) and the HTTP server wind down before the connection pool is closed.
	// shutdown.Notify acks the signal immediately, gives that clean wind-down a
	// chance to complete, and forces an exit on a second interrupt. The deferred
	// stop guarantees the context is always released; a second stop below also
	// waits for the sweep.
	rootCtx, stop := shutdown.Notify(func() {
		log.Printf("interrupt received, shutting down gracefully (Ctrl-C again to force exit)")
	})
	defer stop()

	st, err := store.Open(rootCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	migrateCtx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
	defer cancel()
	if err := st.Migrate(migrateCtx, migrations.FS); err != nil {
		return err
	}
	log.Printf("migrations applied")

	// The parse worker owns every projection write: it rebuilds a session from its
	// raw bytes whenever the session is due (bytes ahead of the last rebuild, or a
	// parser epoch behind the binary's). Parser behavior lives in the binary, so a
	// parser change reaches already-ingested data with no manual step and no schema
	// migration: each session's row is stamped with the epoch it was last rebuilt
	// under, and the worker's initial drain picks up everything a bumped parse.Epoch
	// left behind. The store needs the binary's epoch too, to gate cross-session
	// reads while another instance drains a fleet rebuild this process cannot see.
	st.SetParserEpoch(parse.Epoch)
	worker := parse.NewWorker(st, cfg.ParseWorkers, cfg.SignalsSettleInterval)
	// httpapi.New installs the worker's SSE hooks, so it must run before the
	// worker's first drain can fire them; Run starts further down, right before
	// the server begins listening.
	handler := httpapi.New(st, cfg, worker).Routes()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(rootCtx)
	}()

	// Reclaim orphaned CAS blobs in the background. Deleting a session or
	// re-parsing can leave blobs unreferenced; a periodic sweep keeps the store
	// from accumulating them without any per-write bookkeeping. sweepDone lets
	// shutdown wait for an in-flight sweep before the pool closes.
	sweepDone := make(chan struct{})
	if cfg.SweepInterval > 0 {
		go func() {
			defer close(sweepDone)
			runBackgroundSweep(rootCtx, st, cfg.SweepInterval)
		}()
	} else {
		close(sweepDone)
	}
	// Prune expired Open Graph preview cards on the same footing as the sweep: a
	// background loop that deletes cards older than the cache TTL. The cards are
	// rendered lazily on request and cached, so this only clears cards for overviews
	// nobody is sharing anymore; a live share re-renders on demand. ogDone lets
	// shutdown wait for an in-flight cleanup pass before the pool closes.
	ogDone := make(chan struct{})
	if cfg.OGCleanupInterval > 0 {
		go func() {
			defer close(ogDone)
			runOGCleanup(rootCtx, st, cfg.OGCleanupInterval, cfg.OGCacheTTL)
		}()
	} else {
		close(ogDone)
	}
	// Registered after st.Close so LIFO runs it first: cancel the background loops
	// and wait for them to finish before the pool closes, on every return path
	// (including an early ListenAndServe error). Receiving from an already-closed
	// channel is safe, so this composes with the signal handler's own wait.
	defer func() {
		stop()
		<-sweepDone
		<-ogDone
		<-workerDone
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// These are absolute deadlines sized for the small, fast requests that make
		// up almost all traffic. The two large-body routes (chunk uploads up to
		// 128 MiB, CAS blobs up to 2 GiB) would be truncated mid-stream by these
		// caps on a slow link, so they manage their own idle deadlines via
		// http.NewResponseController instead (see internal/server/httpapi/deadlines.go);
		// the SSE stream does the same for its long-lived writes.
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// On the first interrupt (rootCtx cancelled), stop accepting connections and
	// drain in-flight requests, then wait for the sweep to finish its current
	// pass, before the deferred st.Close shuts the pool.
	idleClosed := make(chan struct{})
	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		<-sweepDone
		<-ogDone
		<-workerDone
		close(idleClosed)
	}()

	if cfg.SweepInterval > 0 {
		log.Printf("background blob sweep every %s", cfg.SweepInterval)
	}
	if cfg.OGCleanupInterval > 0 {
		log.Printf("overview preview cache cleanup every %s", cfg.OGCleanupInterval)
	}
	if cfg.SignalsSettleInterval > 0 {
		log.Printf("parse worker maintenance tick every %s (%d rebuild worker(s))", cfg.SignalsSettleInterval, cfg.ParseWorkers)
	} else {
		log.Printf("parse worker maintenance tick disabled (%d rebuild worker(s), wake-driven only)", cfg.ParseWorkers)
	}
	log.Printf("akari-server listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}
