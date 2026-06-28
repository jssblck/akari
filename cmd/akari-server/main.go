// Command akari-server ingests, stores, parses, and serves agent sessions.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/httpapi"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

func main() {
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
	log.Printf("migrations applied")

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
		close(idleClosed)
	}()

	log.Printf("akari-server listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}
