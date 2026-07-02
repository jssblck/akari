package main

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/storetest"
)

// TestRunSettleMaintenanceDrainsCacheSavings exercises the loop itself, not one pass: with a short
// interval it prices a cache-savings candidate on a tick without any request driving it, and returns
// promptly once its context is cancelled. This is the periodic-drain guarantee the loop exists to
// provide: a pricing rolling deploy keeps minting candidates (an old binary's applyAggregates drops
// cache_savings_backfilled=false on cache-bearing appends after the newer binary's startup drain has
// finished), so a one-shot drain would leave them provisional until an unrelated read. Draining each
// tick consumes them.
func TestRunSettleMaintenanceDrainsCacheSavings(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// A cache-bearing session flagged as a candidate (cache_savings_backfilled=false) with a zero
	// rollup, the state an unfolded or reprice-pending session sits in. The drain must price it.
	var sid int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1, $2, 'claude', 'sess-cache-drain', 'box') RETURNING id`, u.ID, pid).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 200000, 100000, 800000, 0, NULL, now(), 'cd-1')`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET cache_savings_backfilled = false, total_cache_savings_usd = 0 WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		runSettleMaintenance(ctx, st, 10*time.Millisecond)
		close(done)
	}()

	// A tick fires and prices the candidate, flipping cache_savings_backfilled to true; poll for it
	// rather than pinning a single sleep to the ticker period.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var backfilled bool
		if err := st.Pool.QueryRow(ctx,
			"SELECT cache_savings_backfilled FROM sessions WHERE id = $1", sid).Scan(&backfilled); err != nil {
			cancel()
			t.Fatalf("poll candidate flag: %v", err)
		}
		if backfilled {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("settle maintenance loop did not price the cache-savings candidate within 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The priced rollup is now nonzero (the session had cache reads to save on), so the drain wrote a
	// real value, not just flipped the flag.
	var savings float64
	if err := st.Pool.QueryRow(ctx,
		"SELECT total_cache_savings_usd FROM sessions WHERE id = $1", sid).Scan(&savings); err != nil {
		cancel()
		t.Fatalf("read priced rollup: %v", err)
	}
	if savings <= 0 {
		cancel()
		t.Fatalf("priced rollup = %v, want a positive saving for a cache-bearing session", savings)
	}

	// Cancelling the context stops the loop; it must return rather than spin.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("settle maintenance loop did not return after context cancel")
	}
}
