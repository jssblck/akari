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
	// Registered after st.Close so LIFO runs it first: cancel the sweep and wait
	// for it to finish before the pool closes, on every return path (including an
	// early ListenAndServe error). Receiving from an already-closed sweepDone is
	// safe, so this composes with the signal handler's own wait.
	defer func() {
		stop()
		<-sweepDone
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(st, cfg, reparser).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout is generous enough for a bounded (64 MiB) chunk upload on a
		// slow link while still capping slow-loris body reads.
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
		close(idleClosed)
	}()

	if cfg.SweepInterval > 0 {
		log.Printf("background blob sweep every %s", cfg.SweepInterval)
	}
	log.Printf("akari-server listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}
