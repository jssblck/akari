package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestSweepBlobsKeepsCommittedBatchProgress cancels a sweep between its first
// batch's commit and its second batch's, and asserts the first batch stays
// committed and counted, leaving only the interrupted row for a later pass.
//
// The cancel is gated on the store's batch-commit hook, not on polling the
// table: a poll can observe the batch's deletes as soon as the server commits,
// which is before the sweeping client has its commit acknowledgment, and a
// cancel landing in that window aborts the Commit call and drops the durable
// batch from the reported count. The hook fires only after the acknowledgment,
// so cancelling from it is deterministic. The second batch cannot commit
// meanwhile because its delete blocks on an advisory lock the test holds.
func TestSweepBlobsKeepsCommittedBatchProgress(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// One full batch plus a single blocked row in the batch after it. The shas
	// are fixed-width hex, so sha order is numeric order and the blocked row
	// (the highest value) is always the second batch's only member.
	total := store.SweepBlobBatchSize + 1
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO blobs (sha256, lo_oid, byte_len, media_type, content_type)
		SELECT lpad(to_hex(i), 64, '0'),
		       lo_from_bytea(0, convert_to(i::text, 'UTF8')),
		       octet_length(convert_to(i::text, 'UTF8')),
		       'application/octet-stream', 'application/octet-stream'
		  FROM generate_series(1, $1) AS generated(i)`, total); err != nil {
		t.Fatalf("seed orphan blobs: %v", err)
	}

	const lockKey int64 = 781394652
	blockedSHA := fmt.Sprintf("%064x", total)
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION block_last_blob_delete() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
		  IF OLD.sha256 = '%s' THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN OLD;
		END
		$$`, blockedSHA, lockKey)); err != nil {
		t.Fatalf("create blocking delete function: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `
		CREATE TRIGGER block_last_blob_delete
		BEFORE DELETE ON blobs
		FOR EACH ROW EXECUTE FUNCTION block_last_blob_delete()`); err != nil {
		t.Fatalf("create blocking delete trigger: %v", err)
	}

	lockTx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer lockTx.Rollback(context.Background())
	if _, err := lockTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		t.Fatalf("hold delete blocker: %v", err)
	}

	// Buffered past the two commits this test can produce (the full first batch
	// and the final one-row pass) so the hook never blocks the sweep.
	batchCommitted := make(chan int, 4)
	st.SetSweepBatchCommittedHookForTest(func(batchRemoved int) {
		batchCommitted <- batchRemoved
	})

	type sweepResult struct {
		removed int
		err     error
	}
	sweepCtx, cancelSweep := context.WithCancel(ctx)
	defer cancelSweep()
	done := make(chan sweepResult, 1)
	go func() {
		removed, err := st.SweepBlobs(sweepCtx)
		done <- sweepResult{removed: removed, err: err}
	}()

	select {
	case n := <-batchCommitted:
		if n != store.SweepBlobBatchSize {
			t.Fatalf("first sweep batch committed %d removals, want %d", n, store.SweepBlobBatchSize)
		}
	case early := <-done:
		t.Fatalf("sweep returned before its first batch committed: removed=%d err=%v", early.removed, early.err)
	case <-time.After(30 * time.Second):
		t.Fatal("first sweep batch did not commit")
	}
	cancelSweep()

	var result sweepResult
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled sweep did not return")
	}
	if result.err == nil {
		t.Fatal("cancelled sweep returned no error")
	}
	if result.removed != store.SweepBlobBatchSize {
		t.Fatalf("cancelled sweep reported %d committed removals, want %d", result.removed, store.SweepBlobBatchSize)
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release delete blocker: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "DROP TRIGGER block_last_blob_delete ON blobs"); err != nil {
		t.Fatalf("drop blocking trigger: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "DROP FUNCTION block_last_blob_delete()"); err != nil {
		t.Fatalf("drop blocking function: %v", err)
	}

	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("finish sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("finish sweep removed %d blobs, want 1", removed)
	}
}
