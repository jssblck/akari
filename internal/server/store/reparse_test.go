package store

import (
	"context"
	"testing"
)

// TestReparsedEpochRoundTrip confirms a fresh database reports epoch 0 (so the
// server treats its corpus as needing a reparse and converges) and that a write
// is read back.
func TestReparsedEpochRoundTrip(t *testing.T) {
	st := newTestStore(t)
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
// instances from reparsing the same corpus at once.
func TestAcquireReparseLock(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	if !ok {
		t.Fatal("first acquire should succeed on a free lock")
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
	third, ok, err := st.AcquireReparseLock(ctx)
	if err != nil {
		t.Fatalf("acquire third: %v", err)
	}
	if !ok {
		t.Fatal("acquire should succeed after the lock is released")
	}
	third.Release(ctx)
}
