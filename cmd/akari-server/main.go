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

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/httpapi"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/reparse"
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

	// Self-healing reparse. Parser behavior lives in the binary, so a parser change
	// reaches already-ingested data only by replaying the stored raw bytes through
	// the new reducer. The signal is parse.Epoch (a compiled-in constant) compared
	// against the epoch the corpus was last reparsed under: when they differ, a fresh
	// binary rebuilds the projection in the background, with no manual CLI step and no
	// schema migration required (parser changes often ship without one). The service
	// is shared by the startup auto-run here, the admin Reparse button, and the CLI,
	// and an advisory lock keeps multiple instances from reparsing at once. Wait for
	// any in-flight reparse to wind down on shutdown, before the pool closes; it is
	// registered after the deferred st.Close so it runs first (LIFO).
	reparser := reparse.New(rootCtx, st)
	defer reparser.Wait()
	if epoch, err := st.ReparsedEpoch(rootCtx); err != nil {
		log.Printf("reparse: could not read epoch, skipping auto-reparse: %v", err)
	} else if epoch != parse.Epoch {
		log.Printf("reparse: parser epoch %d != stored %d, reparsing in the background", parse.Epoch, epoch)
		reparser.Trigger(reparse.Options{})
	}

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
	// Materialize per-session signals for settled sessions on the same footing as the
	// sweep. The ingest append path deliberately leaves signals uncomputed (see
	// AdvanceProjection) so ingest stays linear and a live session is never graded with a
	// time-dependent outcome; this loop fills the grade in once a session has been idle
	// past the abandoned threshold. settleDone lets shutdown wait for an in-flight pass
	// before the pool closes.
	settleDone := make(chan struct{})
	if cfg.SignalsSettleInterval > 0 {
		go func() {
			defer close(settleDone)
			runSignalsSettle(rootCtx, st, cfg.SignalsSettleInterval)
		}()
	} else {
		close(settleDone)
	}

	// Backfill the per-session cache-savings rollup for any session the parse-time fold never
	// reached: a session ingested before the column existed whose reparse fails keeps its
	// usage_events but a zero rollup, which the epoch reparse cannot fix. The saving is a pure
	// function of usage_events, so this prices it directly, independent of the parse. It is a
	// self-limiting, idempotent one-shot (not a loop), run in the background so a large first
	// pass does not delay accepting connections; backfillDone lets shutdown wait for it.
	backfillDone := make(chan struct{})
	go func() {
		defer close(backfillDone)
		if n, err := st.BackfillCacheSavings(rootCtx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("cache-savings backfill: %v", err)
			}
		} else if n > 0 {
			log.Printf("cache-savings backfill: priced %d session(s)", n)
		}
	}()

	// Registered after st.Close so LIFO runs it first: cancel the background loops
	// and wait for them to finish before the pool closes, on every return path
	// (including an early ListenAndServe error). Receiving from an already-closed
	// channel is safe, so this composes with the signal handler's own wait.
	defer func() {
		stop()
		<-sweepDone
		<-ogDone
		<-settleDone
		<-backfillDone
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(st, cfg, reparser).Routes(),
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
		<-settleDone
		<-backfillDone
		close(idleClosed)
	}()

	if cfg.SweepInterval > 0 {
		log.Printf("background blob sweep every %s", cfg.SweepInterval)
	}
	if cfg.OGCleanupInterval > 0 {
		log.Printf("overview preview cache cleanup every %s", cfg.OGCleanupInterval)
	}
	if cfg.SignalsSettleInterval > 0 {
		log.Printf("signals settle pass every %s", cfg.SignalsSettleInterval)
	}
	log.Printf("akari-server listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}
