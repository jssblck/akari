package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/storetest"
)

// TestReparsedEpochRoundTrip confirms a fresh database reports epoch 0 (so the
// server treats its corpus as needing a reparse and converges) and that a write
// is read back.
func TestReparsedEpochRoundTrip(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	got, err := st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if got != 0 {
		t.Fatalf("fresh database epoch = %d, want 0", got)
	}

	if err := st.SetReparsedEpoch(ctx, 7); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	got, err = st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch after set: %v", err)
	}
	if got != 7 {
		t.Fatalf("epoch after set = %d, want 7", got)
	}

	// The singleton constraint means a second set updates the one row rather than
	// inserting another.
	if err := st.SetReparsedEpoch(ctx, 9); err != nil {
		t.Fatalf("set epoch again: %v", err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM parse_meta").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("parse_meta rows = %d, want 1 (singleton)", rows)
	}
}

// TestAcquireReparseLock confirms the advisory lock is mutually exclusive across
// connections and is reusable after release: this is what keeps two server
// instances from reparsing the same corpus at once. ReparseLockHeld observes the
// same state from any connection, which is how a follower instance gates its
// parsed UI while another instance reparses.
func TestAcquireReparseLock(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	if held, err := st.ReparseLockHeld(ctx); err != nil || held {
		t.Fatalf("lock should be free initially: held=%v err=%v", held, err)
	}

	first, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	if !ok {
		t.Fatal("first acquire should succeed on a free lock")
	}

	if held, err := st.ReparseLockHeld(ctx); err != nil || !held {
		t.Fatalf("lock should read as held while owned: held=%v err=%v", held, err)
	}

	// A second acquire takes a different pooled connection, so the session-scoped
	// advisory lock is already held: it must fail without blocking.
	second, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire second: %v", err)
	}
	if ok {
		second.Release(ctx)
		t.Fatal("second acquire should fail while the lock is held")
	}

	// After release the lock is free again.
	first.Release(ctx)
	if held, err := st.ReparseLockHeld(ctx); err != nil || held {
		t.Fatalf("lock should read as free after release: held=%v err=%v", held, err)
	}
	third, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire third: %v", err)
	}
	if !ok {
		t.Fatal("acquire should succeed after the lock is released")
	}
	third.Release(ctx)
}
