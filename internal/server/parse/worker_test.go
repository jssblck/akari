package parse

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestWorkerDrainRebuildsDueSessions drives the worker's synchronous drain over
// a fresh two-session corpus: both sessions are due (never rebuilt, parser_epoch
// 0), the drain rebuilds them through the real reducer, fires the rebuilt hook
// per session, and reports a completed fleet status (a fresh corpus is an
// epoch-stale backlog, so the drain runs in fleet mode). Afterward FleetStatus
// reads not-in-progress: the corpus is current.
func TestWorkerDrainRebuildsDueSessions(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	sidA := seedSession(t, st, "worker-a")
	// seedSession registers the first admin, so the second session reuses the
	// same user through a direct announce.
	var uid int64
	if err := st.Pool.QueryRow(ctx, "SELECT user_id FROM sessions WHERE id = $1", sidA).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := st.Pool.QueryRow(ctx, "SELECT project_id FROM sessions WHERE id = $1", sidA).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	annB, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: "worker-b", ProjectID: pid,
	})
	if err != nil {
		t.Fatal(err)
	}
	sidB := annB.SessionID

	whole := claudeLines[0] + claudeLines[1] + claudeLines[2]
	for _, sid := range []int64{sidA, sidB} {
		if _, err := st.AppendChunk(ctx, sid, 0, []byte(whole)); err != nil {
			t.Fatalf("append to %d: %v", sid, err)
		}
	}

	w := NewWorker(st, 2, 0)
	var mu sync.Mutex
	var rebuilt []int64
	w.SetRebuiltHook(func(sessionID int64) {
		mu.Lock()
		rebuilt = append(rebuilt, sessionID)
		mu.Unlock()
	})

	status, err := w.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if status.InProgress || status.Total != 2 || status.Done != 2 || status.Failed != 0 {
		t.Fatalf("drain status = %+v, want a completed 2/2 fleet rebuild", status)
	}
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i] < rebuilt[j] })
	want := []int64{sidA, sidB}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(rebuilt) != 2 || rebuilt[0] != want[0] || rebuilt[1] != want[1] {
		t.Fatalf("rebuilt hook fired for %v, want %v", rebuilt, want)
	}
	for _, sid := range []int64{sidA, sidB} {
		if mc := messageCount(t, st, sid); mc != 2 {
			t.Errorf("session %d message_count = %d, want 2 after the drain's rebuild", sid, mc)
		}
	}
	if fs := w.FleetStatus(ctx); fs.InProgress {
		t.Fatalf("FleetStatus after a full drain = %+v, want not in progress", fs)
	}
}

// TestWorkerTriggerRedrains pins the operator path: Trigger marks the scope due
// again (regardless of the epoch), and the next drain rebuilds it as a fresh
// fleet pass. This is the admin Reparse button and the CLI in miniature.
func TestWorkerTriggerRedrains(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	sid := seedSession(t, st, "worker-trigger")
	whole := claudeLines[0] + claudeLines[1] + claudeLines[2]
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(whole)); err != nil {
		t.Fatal(err)
	}

	w := NewWorker(st, 1, 0)
	if status, err := w.Drain(ctx); err != nil || status.Done != 1 {
		t.Fatalf("initial drain = %+v (err %v), want 1 done", status, err)
	}

	marked, err := w.Trigger(ctx, "")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if marked != 1 {
		t.Fatalf("trigger marked %d sessions, want 1", marked)
	}
	// The forced-due corpus reads as a fleet rebuild until the drain runs.
	if fs := w.FleetStatus(ctx); !fs.InProgress {
		t.Fatal("FleetStatus after Trigger should report in progress (the corpus is epoch-stale)")
	}
	if status, err := w.Drain(ctx); err != nil || status.Done != 1 || status.Failed != 0 || status.InProgress {
		t.Fatalf("post-trigger drain = %+v (err %v), want a completed 1/1 pass", status, err)
	}
	if fs := w.FleetStatus(ctx); fs.InProgress {
		t.Fatalf("FleetStatus after redrain = %+v, want not in progress", fs)
	}
}

// TestWorkerDrainGrowsTotalForMidDrainArrivals pins the progress arithmetic
// when live traffic lands during a fleet rebuild: a session announced mid-drain
// starts at parser_epoch 0, so it joins the epoch-stale backlog after the drain
// counted its starting total. The denominator must grow to cover it rather than
// Done running past Total (the 6-of-1 progress bar this once produced).
func TestWorkerDrainGrowsTotalForMidDrainArrivals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	sidA := seedSession(t, st, "mid-drain-a")
	var uid, pid int64
	if err := st.Pool.QueryRow(ctx, "SELECT user_id, project_id FROM sessions WHERE id = $1", sidA).Scan(&uid, &pid); err != nil {
		t.Fatal(err)
	}

	// The rebuilt hook runs synchronously inside the drain, so announcing here
	// lands a new due session between the drain's pages, exactly the live-ingest
	// interleaving the arithmetic has to absorb.
	w := NewWorker(st, 1, 0)
	announced := false
	w.SetRebuiltHook(func(int64) {
		if announced {
			return
		}
		announced = true
		if _, err := st.Announce(ctx, store.AnnounceParams{
			UserID: uid, Agent: "claude", SourceSessionID: "mid-drain-b", ProjectID: pid,
		}); err != nil {
			t.Errorf("mid-drain announce: %v", err)
		}
	})

	status, err := w.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if status.Total != 2 || status.Done != 2 || status.Failed != 0 || status.InProgress {
		t.Fatalf("drain status = %+v, want the mid-drain arrival absorbed as a completed 2/2", status)
	}
}

// TestWorkerDrainReportsOperationalFailure pins the foreground contract the
// reparse CLI relies on: an operational rebuild failure (here a tool input
// lifted to the CAS whose blob was never uploaded, so the rebuild rolls back
// mid-transaction) surfaces as Drain's error rather than an exit-zero "done",
// and the session stays due so a later drain can complete it. A deterministic
// parser failure would instead be recorded and counted in Status.Failed; this
// one must not be, since nothing was recorded and the work is not done.
func TestWorkerDrainReportsOperationalFailure(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	sid := seedSession(t, st, "worker-op-fail")
	missingBlob := `{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"id":"msg_op","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"toolu_op","name":"Read","input":{"__akari_cas__":1,"sha256":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef","bytes":12,"media_type":"application/json"}}]}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(claudeLines[0]+missingBlob)); err != nil {
		t.Fatal(err)
	}

	status, err := NewWorker(st, 1, 0).Drain(ctx)
	if err == nil {
		t.Fatal("Drain swallowed an operational rebuild failure; the reparse CLI would report success")
	}
	if status.Failed != 0 {
		t.Fatalf("status.Failed = %d, want 0 (an operational failure is not a recorded parser failure)", status.Failed)
	}
	// The failure recorded a retry backoff, so the session is deferred (not in
	// the due scan right now) but the work is still incomplete and the retry
	// path is intact: once the backoff elapses it is due again.
	isDue := func() bool {
		t.Helper()
		due, err := st.DueSessions(ctx, Epoch, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			if d.ID == sid {
				return true
			}
		}
		return false
	}
	if isDue() {
		t.Fatal("operationally-failed session should be backing off, not immediately due (it would retry on every wake)")
	}
	var parsed, byteLen int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT parsed_byte_len, byte_len FROM session_raw WHERE session_id = $1
		    AND parse_retry_at > now()`, sid).Scan(&parsed, &byteLen); err != nil {
		t.Fatalf("read backoff state: %v (want a future parse_retry_at)", err)
	}
	if parsed == byteLen {
		t.Fatal("operational failure advanced the parse cursor; the work would read as done")
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE session_raw SET parse_retry_at = now() - interval '1 second' WHERE session_id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	if !isDue() {
		t.Fatal("operationally-failed session not due after its backoff elapsed; the retry path is gone")
	}
}

// TestWorkerFleetStatusSeesForeignBacklog pins the cross-instance gate and its
// cache asymmetry: a worker that has run no drain itself (its local status is
// idle) still reports a fleet rebuild in progress when the shared corpus holds
// epoch-stale sessions, which is how a follower instance gates its parsed pages
// while another instance drains. The positive answer is served from cache for
// the TTL (briefly over-gating after the backlog clears is the documented safe
// side); a negative is never cached, so once the TTL lapses the follower
// re-reads and ungates.
func TestWorkerFleetStatusSeesForeignBacklog(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	sid := seedSession(t, st, "worker-foreign")
	whole := claudeLines[0] + claudeLines[1] + claudeLines[2]
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(whole)); err != nil {
		t.Fatal(err)
	}

	// The follower: no drain has run in this worker, but the corpus (one session
	// still at parser_epoch 0) is behind the epoch. An effectively infinite TTL
	// makes the caching observable without racing the clock.
	follower := NewWorker(st, 1, 0)
	follower.fleetTTL = time.Hour
	if fs := follower.FleetStatus(ctx); !fs.InProgress {
		t.Fatal("follower FleetStatus should report the shared epoch-stale backlog")
	}
	if s := follower.Status(); s.InProgress {
		t.Fatal("the follower's own status must stay idle; only FleetStatus widens")
	}

	// Another instance drains the backlog. The follower's cached positive still
	// gates (over-gating inside the TTL is allowed) until the TTL lapses, at
	// which point the fresh read sees the drained corpus.
	NewWorker(st, 1, 0).Drain(ctx)
	if fs := follower.FleetStatus(ctx); !fs.InProgress {
		t.Fatal("inside the TTL the cached positive answer should still gate")
	}
	follower.fleetTTL = 0
	if fs := follower.FleetStatus(ctx); fs.InProgress {
		t.Fatalf("follower FleetStatus after the TTL lapsed = %+v, want a fresh not-in-progress read", fs)
	}
}
