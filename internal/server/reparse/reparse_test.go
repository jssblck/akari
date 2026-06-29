package reparse

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// claudeSession is a minimal one-line Claude transcript, enough to produce a
// message and a usage row when parsed.
const claudeSession = `{"type":"assistant","timestamp":"2024-01-01T10:00:00Z","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"

// seedProject registers the bootstrap admin (the only invite-free registration a
// fresh schema allows) and a project, returning their ids for seeding sessions.
func seedProject(t *testing.T, st *store.Store) (uid, projectID int64) {
	t.Helper()
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	return u.ID, pid
}

// seedSession announces a Claude session, stores one chunk, and parses it, so the
// session has a real projection a reparse can rebuild.
func seedSession(t *testing.T, st *store.Store, uid, projectID int64, source string) int64 {
	t.Helper()
	ctx := context.Background()
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: source, ProjectID: projectID,
		GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce %s: %v", source, err)
	}
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte(claudeSession)); err != nil {
		t.Fatalf("append %s: %v", source, err)
	}
	if _, err := parse.Advance(ctx, st, ann.SessionID, "claude"); err != nil {
		t.Fatalf("advance %s: %v", source, err)
	}
	return ann.SessionID
}

// TestRunStampsEpoch confirms a completed full reparse records parse.Epoch, so the
// next startup sees the epochs match and does not reparse again. An empty corpus is
// enough: the service still runs the loop (over zero sessions) and writes the epoch.
func TestRunStampsEpoch(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Sanity: a fresh database is "stale" (0 != parse.Epoch), which is what makes a
	// brand-new server reparse on first boot.
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch == parse.Epoch {
		t.Fatalf("fresh epoch = %d (err %v), want a value != parse.Epoch (%d)", epoch, err, parse.Epoch)
	}

	svc := New(ctx, st)
	res, err := svc.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.InProgress {
		t.Fatal("status should not be in progress after Run returns")
	}

	epoch, err := st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != parse.Epoch {
		t.Fatalf("reparsed_epoch = %d, want parse.Epoch (%d)", epoch, parse.Epoch)
	}
}

// TestRunReparsesSeededSessionsAcrossPages seeds more sessions than one page holds,
// shrinks the page size, and confirms the loop pages through all of them: progress
// counts reach Total, the epoch is stamped, and the projection survives.
func TestRunReparsesSeededSessionsAcrossPages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid, pid := seedProject(t, st)

	const n = 5
	for i := 0; i < n; i++ {
		seedSession(t, st, uid, pid, fmt.Sprintf("s-%d", i))
	}

	svc := New(ctx, st)
	// Force multi-page paging without seeding hundreds of sessions. Setting it on this
	// service (not a shared global) keeps the parallel tests race-free.
	svc.pageSize = 2
	res, err := svc.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Total != n || res.Done != n || res.Failed != 0 {
		t.Fatalf("result = %+v, want Total=%d Done=%d Failed=0", res, n, n)
	}

	// Every session still has its message after the in-place rebuild.
	var messages int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != n {
		t.Fatalf("messages after reparse = %d, want %d", messages, n)
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != parse.Epoch {
		t.Fatalf("epoch after reparse = %d (err %v), want parse.Epoch (%d)", epoch, err, parse.Epoch)
	}
}

// TestAgentFilteredRunDoesNotStampEpoch confirms a partial (agent-filtered) reparse
// leaves the epoch behind, so a targeted CLI run never claims the whole corpus is
// current.
func TestAgentFilteredRunDoesNotStampEpoch(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	if _, err := svc.Run(ctx, Options{Agent: "claude"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch after agent-filtered run = %d (err %v), want 0 (unchanged)", epoch, err)
	}
}

// TestTriggerSerialized confirms a reparse already in progress makes a second
// Trigger a no-op that returns the running status rather than starting a second
// run. The first run is held inside its initial progress emit so the window stays
// open deterministically.
func TestTriggerSerialized(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	svc.SetProgressHook(func(Status) {
		// Block the very first emit (the start of execute, before any work), holding
		// InProgress=true for the duration of the test.
		once.Do(func() {
			close(started)
			<-release
		})
	})

	svc.Trigger(Options{})
	<-started

	if !svc.Status().InProgress {
		t.Fatal("status should be in progress while the first run is held")
	}
	// A second Trigger must not start a new run; it returns the running status.
	if got := svc.Trigger(Options{}); !got.InProgress {
		t.Fatal("second Trigger should return the running status (in progress)")
	}

	close(release)
	svc.Wait()

	if svc.Status().InProgress {
		t.Fatal("status should clear after the run completes")
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != parse.Epoch {
		t.Fatalf("epoch after completion = %d (err %v), want parse.Epoch (%d)", epoch, err, parse.Epoch)
	}
}

// TestSkipsWhenLockHeld confirms the service skips (and does not stamp the epoch)
// when another holder owns the advisory lock, the cross-process guard that keeps
// two instances from reparsing at once. A follower instead gates its UI via
// FleetStatus, checked next.
func TestSkipsWhenLockHeld(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Stand in for another instance by holding the lock on a separate connection.
	held, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("pre-hold lock: ok=%v err=%v", ok, err)
	}
	defer held.Release(ctx)

	svc := New(ctx, st)
	if _, err := svc.Run(ctx, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if svc.Status().InProgress {
		t.Fatal("status should be cleared after a skipped run")
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch after skipped run = %d (err %v), want 0 (lock held, no reparse)", epoch, err)
	}
}

// TestFleetStatusReflectsHeldLock confirms a follower instance, not itself
// reparsing, still reports in-progress (and so gates its parsed UI) while another
// instance holds the reparse lock, and reports idle once the lock frees.
func TestFleetStatusReflectsHeldLock(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	// Read the lock state through on every check rather than caching it, so this test
	// observes the idle -> held -> released transitions immediately instead of being
	// masked by the gating TTL (the first idle read would otherwise cache "not held"
	// across the acquire below). Setting it on this service, not a shared global, keeps
	// the parallel tests race-free.
	svc.fleetTTL = 0
	if svc.FleetStatus(ctx).InProgress {
		t.Fatal("fleet status should be idle with no reparse running")
	}

	held, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("hold lock: ok=%v err=%v", ok, err)
	}
	// Release on every exit, including a failed assertion: an unreleased lock keeps
	// its pooled connection pinned, which would wedge the pool's Close in cleanup.
	defer held.Release(ctx)

	if !svc.FleetStatus(ctx).InProgress {
		t.Fatal("fleet status should report in-progress while another instance holds the lock")
	}

	held.Release(ctx)
	if svc.FleetStatus(ctx).InProgress {
		t.Fatal("fleet status should report idle once the lock is released")
	}
}

// TestFleetStatusDoesNotCacheNegative confirms a follower that just saw the lock free
// still detects a reparse that starts immediately after, so it cannot serve parsed
// pages during another instance's reparse. It runs at the default (non-zero) TTL on
// purpose: a cached "not held" would mask the freshly taken lock for the TTL window,
// which is the regression this guards. Only a positive result may be cached.
func TestFleetStatusDoesNotCacheNegative(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	// An idle check; with a negative cache this would pin "not held" for the whole TTL.
	if svc.FleetStatus(ctx).InProgress {
		t.Fatal("fleet status should be idle before any lock is held")
	}

	// Another instance takes the lock right after the idle check. The next check must
	// read it fresh rather than return a cached "not held".
	held, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("hold lock: ok=%v err=%v", ok, err)
	}
	defer held.Release(ctx)

	if !svc.FleetStatus(ctx).InProgress {
		t.Fatal("fleet status must detect a lock taken right after an idle check; the not-held result must not be cached")
	}
}
