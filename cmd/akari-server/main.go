// Command akari-server ingests, stores, parses, and serves agent sessions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/httpapi"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/version"
	"github.com/jssblck/akari/migrations"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version.String())
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

	// rootCtx is cancelled on shutdown so background work (the blob sweep) stops
	// before the connection pool is closed. The deferred stop guarantees the
	// context is always released; a second stop below also waits for the sweep.
	rootCtx, stop := context.WithCancel(context.Background())
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
		Handler:           httpapi.New(st, cfg).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout is generous enough for a bounded (64 MiB) chunk upload on a
		// slow link while still capping slow-loris body reads.
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on interrupt.
	idleClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		// Stop the background sweep and let it finish its current pass before the
		// pool is closed by the deferred st.Close.
		stop()
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
