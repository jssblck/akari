package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/devseed"
	"github.com/jssblck/akari/internal/server/store"
)

// runDevLaunch runs the server for local preview/debug with its dev dependencies
// wrapped around it. The .claude launch config invokes it.
//
// The launch config runs the server here in the foreground so the preview
// launcher can track it on its assigned $PORT. That path (a direct run, not
// `eph up`) starts no database, runs no seed, and tears nothing down, so this
// supplies those pieces:
//
//  1. `eph up postgres` (blocks until healthy) and load the now-resolved
//     AKARI_DATABASE_URL into the environment,
//  2. seed example data once the server reports healthy, in the background and
//     best-effort (the same idempotent dev-seed the eph post-start hook runs),
//  3. run the server in the foreground, reusing the normal startup (migrations,
//     reparse, sweep, graceful shutdown on signal), and
//  4. `eph down` when the launch ends, which stops the containers but keeps the
//     pgdata volume, so the next launch restarts fast and stays seeded.
//
// It shells out to the eph CLI (already required for local dev) and otherwise
// reuses the server's own code, so there is no second build step. A forced
// second Ctrl-C exits before the deferred `eph down` runs; the next launch just
// reuses the still-running stack.
func runDevLaunch(args []string) error {
	fs := flag.NewFlagSet("dev-launch", flag.ContinueOnError)
	noSeed := fs.Bool("no-seed", false, "skip seeding example data (just bring up Postgres and run the server)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// ctx is cancelled when this function returns, which stops the background
	// seeder if the server is interrupted mid-seed. The server's own startup
	// installs the signal handler that ends the foreground run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Postgres only; the server runs here in the foreground, not as an eph
	// service. `eph up` blocks until Postgres is healthy.
	if err := eph(ctx, "up", "postgres"); err != nil {
		return fmt.Errorf("eph up postgres: %w", err)
	}
	// Tear the stack down when the launch ends, however it ends. `eph down` keeps
	// the named pgdata volume, so a restart is quick and the data stays seeded.
	defer func() {
		if err := eph(context.Background(), "down"); err != nil {
			log.Printf("dev-launch: eph down: %v", err)
		}
	}()

	// Load AKARI_DATABASE_URL (now resolved against the running Postgres) into the
	// environment so the in-process server and seeder pick it up.
	if err := loadEphEnv(ctx); err != nil {
		return err
	}

	if !*noSeed {
		// config.LoadServer needs AKARI_DATABASE_URL, set just above. The seeder
		// reaches the server at the same address the server is about to bind.
		cfg, err := config.LoadServer()
		if err != nil {
			return err
		}
		go seedWhenHealthy(ctx, deriveServerURL(cfg.Listen))
	}

	// Run the server in the foreground, reusing the normal startup. It returns
	// when the launch is interrupted, at which point the deferred eph down runs.
	return run()
}

// eph runs the eph CLI with stdout/stderr attached so its progress and errors
// show in the launch output. eph is required for local dev and expected on PATH.
func eph(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "eph", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// loadEphEnv runs `eph env` and exports every fully-resolved variable into this
// process's environment, so the in-process server and seeder see the same
// connection details `eph env` would give a shell.
func loadEphEnv(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "eph", "env", "-f", "json").Output()
	if err != nil {
		return fmt.Errorf("eph env: %w", err)
	}
	vars, err := parseEphEnv(out)
	if err != nil {
		return err
	}
	for k, v := range vars {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// parseEphEnv reads `eph env -f json` output and returns only the variables that
// fully resolved. A variable that still references a service which is not running
// keeps an unresolved ${svc.port} placeholder (for example AKARI_URL points at
// the eph server service, which dev-launch does not start); skipping those keeps
// a stale, unusable value from reaching the server or seeder.
func parseEphEnv(data []byte) (map[string]string, error) {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse eph env: %w", err)
	}
	resolved := make(map[string]string, len(raw))
	for k, v := range raw {
		if strings.Contains(v, "${") {
			continue
		}
		resolved[k] = v
	}
	return resolved, nil
}

// seedWhenHealthy waits for the server to pass its health check, then seeds
// example data. The first `go run` of the server can spend tens of seconds
// compiling, so it polls for up to two minutes. Seeding is best-effort: a failure
// is logged and the launch carries on.
func seedWhenHealthy(ctx context.Context, serverURL string) {
	healthz := strings.TrimRight(serverURL, "/") + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}

	healthy := false
	for i := 0; i < 120; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthz, nil)
		if err != nil {
			log.Printf("dev-launch: seed: %v (continuing)", err)
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if !healthy {
		log.Printf("dev-launch: server was not healthy after 120s; skipping seed")
		return
	}

	if err := seed(ctx, serverURL); err != nil {
		log.Printf("dev-launch: seed: %v (continuing)", err)
	}
}

// seed fills the running server with example data. It mirrors the dev-seed
// post-start hook's defaults and is idempotent, so it no-ops once the database
// already holds sessions.
func seed(ctx context.Context, serverURL string) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// The foreground server has already applied migrations by the time the health
	// check passes, so opening the store is enough.
	return devseed.Run(ctx, st, devseed.Options{
		ServerURL:   serverURL,
		NumUsers:    4,
		Password:    "akari-dev",
		TimeLimit:   30 * time.Second,
		Concurrency: 8,
	})
}
