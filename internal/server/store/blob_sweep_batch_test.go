package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/storetest"
)

// TestSweepBlobsKeepsCommittedBatchProgress cancels a sweep while its second
// batch is blocked. The first batch must remain committed and be included in
// the returned count, leaving only the interrupted row for a later pass.
func TestSweepBlobsKeepsCommittedBatchProgress(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO blobs (sha256, lo_oid, byte_len, media_type, content_type)
		SELECT lpad(to_hex(i), 64, '0'),
		       lo_from_bytea(0, convert_to(i::text, 'UTF8')),
		       octet_length(convert_to(i::text, 'UTF8')),
		       'application/octet-stream', 'application/octet-stream'
		  FROM generate_series(1, 257) AS generated(i)`); err != nil {
		t.Fatalf("seed orphan blobs: %v", err)
	}

	const lockKey int64 = 781394652
	if _, err := st.Pool.Exec(ctx, `
		CREATE FUNCTION block_last_blob_delete() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
		  IF OLD.sha256 = lpad(to_hex(257), 64, '0') THEN
		    PERFORM pg_advisory_xact_lock(781394652);
		  END IF;
		  RETURN OLD;
		END
		$$`); err != nil {
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

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case early := <-done:
			t.Fatalf("sweep returned before second batch was cancelled: removed=%d err=%v", early.removed, early.err)
		case <-timeout.C:
			t.Fatal("first sweep batch did not commit")
		case <-ticker.C:
			var remaining int
			if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM blobs").Scan(&remaining); err != nil {
				t.Fatalf("count remaining blobs: %v", err)
			}
			if remaining == 1 {
				cancelSweep()
				goto cancelled
			}
		}
	}

cancelled:
	var result sweepResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled sweep did not return")
	}
	if result.err == nil {
		t.Fatal("cancelled sweep returned no error")
	}
	if result.removed != 256 {
		t.Fatalf("cancelled sweep reported %d committed removals, want 256", result.removed)
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
