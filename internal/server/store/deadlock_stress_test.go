package store_test

// Stress reproduction for the 2026-07-07 production deadlock: two concurrent
// RebuildSession transactions on sessions sharing content-addressed blobs
// deadlocked between pinSessionBlobsTx's INSERT INTO blob_pins and
// refreshSignalsTx's UPDATE sessions SET signals_stale. This test recreates the
// epoch-drain mix (concurrent rebuilds of settled, blob-sharing sessions, plus
// live appends, announces of subagent families, client-CAS pinning, sweeps,
// finalize refreshes, and a dev-seed-style bulk reassign) and logs the full
// Postgres deadlock DETAIL for every 40P01 it provokes, so the exact lock cycle
// is named by the server rather than inferred.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// stressWideStore opens a second Store over the same test database with a pool
// wide enough for the stress mix; storetest caps its pool at 4 connections,
// which would serialize the very concurrency this test exists to provoke.
func stressWideStore(t *testing.T, st *store.Store, conns int) *store.Store {
	t.Helper()
	raw := st.Pool.Config().ConnConfig.ConnString()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse test store conn string %q: %v", raw, err)
	}
	q := u.Query()
	q.Set("pool_max_conns", strconv.Itoa(conns))
	u.RawQuery = q.Encode()
	wide, err := store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatalf("open wide store: %v", err)
	}
	t.Cleanup(wide.Close)
	return wide
}

// stressDelta builds a settled session's delta whose tool calls reference the
// shared body pool (identical content across sessions, so the CAS dedupes them
// to shared blobs and every rebuild pins the same blob_pins rows) plus one
// session-unique body. The message and usage volume is deliberately padded so
// the rebuild's grading phase (gatherSignalFacts and friends) takes long enough
// to overlap other rebuilds' pin phase.
func stressDelta(sessionIdx int, shared []string) store.ProjectionDelta {
	ended := time.Now().Add(-2 * time.Hour)
	started := ended.Add(-30 * time.Minute)
	var msgs []store.MessageDelta
	var usage []store.ProjUsage
	const turns = 40
	for m := 0; m < turns; m++ {
		role := "user"
		content := fmt.Sprintf("please fix failing store test number %d with proper context", m)
		if m%2 == 1 {
			role = "assistant"
			content = strings.Repeat("working on it. ", 20)
		}
		ts := started.Add(time.Duration(m) * 30 * time.Second)
		msgs = append(msgs, store.MessageDelta{
			Ordinal: m, Role: role, Content: content,
			HasToolUse: role == "assistant", HasThinking: role == "assistant",
			ThinkingText: "hmm", ThinkingBytes: 2048, Model: "claude-fable-5", Timestamp: ts,
		})
		if role == "assistant" {
			ord := m
			cost := 0.02
			usage = append(usage, store.ProjUsage{
				MessageOrdinal: &ord, Model: "claude-fable-5",
				Input: 1200, Output: 400, CacheWrite: 100, CacheRead: 9000, Reasoning: 200,
				CostUSD: cost, OccurredAt: ts,
				DedupKey: fmt.Sprintf("turn-%d", m), SourceOffset: int64(m * 100), SourceIndex: 0,
			})
		}
	}
	var calls []store.ProjToolCall
	var results []store.ToolResultDelta
	for j, body := range shared {
		uid := fmt.Sprintf("call-%d", j)
		calls = append(calls, store.ProjToolCall{
			MessageOrdinal: 1, CallIndex: j, ToolName: "Read", Category: "read",
			InputBody: body, InputBytes: int64(len(body)), InputMediaType: "application/json",
			CallUID: uid,
		})
		results = append(results, store.ToolResultDelta{
			CallUID: uid, Body: "result " + body, Bytes: int64(len(body)) + 7,
			MediaType: "text/plain", Status: "ok",
		})
	}
	unique := fmt.Sprintf(`{"session_only":"body for session %d"}`, sessionIdx)
	calls = append(calls, store.ProjToolCall{
		MessageOrdinal: 3, CallIndex: 0, ToolName: "Write", Category: "edit",
		FilePath:  "/home/grace/akari/main.go",
		InputBody: unique, InputBytes: int64(len(unique)), InputMediaType: "application/json",
		CallUID: "call-unique",
	})
	return store.ProjectionDelta{
		Messages:    msgs,
		ToolCalls:   calls,
		ToolResults: results,
		Usage:       usage,
		Started:     started,
		Ended:       ended,
	}
}

// logDeadlock fails the test on a 40P01, dumping every field Postgres puts on
// it so the cycle is named verbatim (the DETAIL lists each process in the
// cycle; the server log pairs each with its query). Any deadlock is a bug: the
// store's writers are supposed to acquire shared rows in one global order.
func logDeadlock(t *testing.T, who string, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		return false
	}
	t.Errorf("DEADLOCK in %s:\n  message: %s\n  detail: %s\n  hint: %s\n  where: %s",
		who, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where)
	return true
}

func TestConcurrentRebuildDeadlockStress(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	const (
		nSessions = 12
		nShared   = 24
		rebuilder = 10 // concurrent rebuild workers
	)
	dur := 20 * time.Second
	if v := os.Getenv("AKARI_DEADLOCK_STRESS_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			dur = time.Duration(n) * time.Second
		}
	}

	// 24 connections covers the mix's peak concurrency (about 19 goroutines that
	// each hold at most one connection) while staying inside the suite's shared
	// Postgres connection budget (see storetest/gate.go).
	wide := stressWideStore(t, st, 24)

	u, err := wide.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	userIDs := []int64{u.ID}
	projectID, err := wide.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	shared := make([]string, nShared)
	for j := range shared {
		shared[j] = fmt.Sprintf(`{"shared_body":"content-addressed tool input %d shared across sessions"}`, j)
	}
	sharedSHAs := make([]string, nShared)
	for j, b := range shared {
		sharedSHAs[j] = store.HashString(b)
	}

	// Sessions come in subagent families: even indexes are parents, odd indexes
	// are their children, so announce-time linking (cross-row sessions updates)
	// runs concurrently with the rebuilds the way live Claude ingest does.
	source := func(i int) string {
		if i%2 == 1 {
			return fmt.Sprintf("stress-sess-%d/subagents/agent-%d", i-1, i)
		}
		return fmt.Sprintf("stress-sess-%d", i)
	}
	announce := func(i int) (store.AnnounceResult, error) {
		return wide.Announce(ctx, store.AnnounceParams{
			UserID: u.ID, Agent: "claude",
			SourceSessionID: source(i),
			ProjectID:       projectID, Kind: "remote",
			GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
		})
	}
	sids := make([]int64, nSessions)
	for i := range sids {
		ann, err := announce(i)
		if err != nil {
			t.Fatalf("announce %d: %v", i, err)
		}
		sids[i] = ann.SessionID
		if err := wide.RebuildSession(ctx, ann.SessionID, testEpoch, stubReducer{stressDelta(i, shared)}); err != nil {
			t.Fatalf("seed rebuild %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(dur)
	var deadlocks, rebuilds, appends, announces, reassigns, pins, sweeps, finalizes atomic.Int64
	stop := func() bool { return time.Now().After(deadline) }
	var wg sync.WaitGroup

	// Rebuild workers: the epoch-drain shape, each rebuilding a random session.
	for w := 0; w < rebuilder; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for !stop() {
				i := rng.Intn(nSessions)
				err := wide.RebuildSession(ctx, sids[i], testEpoch, stubReducer{stressDelta(i, shared)})
				rebuilds.Add(1)
				if err != nil {
					if logDeadlock(t, fmt.Sprintf("RebuildSession(%d)", sids[i]), err) {
						deadlocks.Add(1)
					} else {
						t.Errorf("rebuild %d: %v", sids[i], err)
						return
					}
				}
			}
		}(w)
	}

	// Live-ingest appenders: line-aligned junk chunks onto every session.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(100 + w)))
			offsets := map[int64]int64{}
			for !stop() {
				sid := sids[rng.Intn(nSessions)]
				chunk := []byte("{}\n")
				n, err := wide.AppendChunk(ctx, sid, offsets[sid], chunk)
				var mismatch store.OffsetMismatchError
				switch {
				case err == nil:
					offsets[sid] = n
					appends.Add(1)
				case errors.As(err, &mismatch):
					offsets[sid] = mismatch.StoredBytes
				case logDeadlock(t, fmt.Sprintf("AppendChunk(%d)", sid), err):
					deadlocks.Add(1)
				default:
					t.Errorf("append %d: %v", sid, err)
					return
				}
			}
		}(w)
	}

	// Announce churn: dev-seed and the watch loop re-announce live sessions,
	// including subagent children whose linking updates other sessions' rows.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(200))
		for !stop() {
			if _, err := announce(rng.Intn(nSessions)); err != nil {
				if logDeadlock(t, "Announce", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("announce: %v", err)
					return
				}
			}
			announces.Add(1)
		}
	}()

	// Client-CAS pinners: dev-seed's upload path checks and pins the shared
	// bodies (MissingBlobs) and re-uploads some (PutBlob pins too).
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(400))
		for !stop() {
			subset := make([]string, 0, nShared)
			for _, sha := range sharedSHAs {
				if rng.Intn(2) == 0 {
					subset = append(subset, sha)
				}
			}
			if len(subset) == 0 {
				continue
			}
			if _, err := wide.MissingBlobs(ctx, subset); err != nil {
				if logDeadlock(t, "MissingBlobs", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("missing blobs: %v", err)
					return
				}
			}
			pins.Add(1)
			j := rng.Intn(nShared)
			if err := wide.PutBlob(ctx, sharedSHAs[j], "application/json", "application/octet-stream",
				strings.NewReader(shared[j])); err != nil {
				if logDeadlock(t, "PutBlob", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("put blob: %v", err)
					return
				}
			}
		}
	}()

	// Blob sweeps: the periodic reclaim (expired-pin DELETE runs in heap order,
	// the one blob_pins writer outside the sorted discipline).
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(500))
		for !stop() {
			// Age a random slice of pins so the sweep's expired-pin DELETE has
			// work. The subquery locks the rows in sha order (the pinners'
			// order), so this helper cannot itself manufacture a cycle the
			// production writers would never form.
			if _, err := wide.Pool.Exec(ctx,
				`UPDATE blob_pins p SET expires_at = now() - interval '1 minute'
				   FROM (SELECT sha256 FROM blob_pins WHERE sha256 = ANY($1)
				          ORDER BY sha256 FOR UPDATE) aged
				  WHERE p.sha256 = aged.sha256`, sharedSHAs[:rng.Intn(nShared)+1]); err != nil {
				if logDeadlock(t, "age pins", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("age pins: %v", err)
					return
				}
			}
			if _, err := wide.SweepBlobs(ctx); err != nil {
				if logDeadlock(t, "SweepBlobs", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("sweep: %v", err)
					return
				}
			}
			sweeps.Add(1)
			time.Sleep(30 * time.Millisecond)
		}
	}()

	// Finalize refreshes: `akari sync --finalize` grading sessions on demand.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(600))
		for !stop() {
			sid := sids[rng.Intn(nSessions)]
			if err := wide.RefreshSessionSignals(ctx, sid); err != nil {
				if logDeadlock(t, fmt.Sprintf("RefreshSessionSignals(%d)", sid), err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("finalize %d: %v", sid, err)
					return
				}
			}
			finalizes.Add(1)
		}
	}()

	// Dev-seed reassign: one transaction updating every session's owner row by
	// row, the cross-session lock holder from reassignSessions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(300))
		for !stop() {
			err := pgx.BeginFunc(ctx, wide.Pool, func(tx pgx.Tx) error {
				order := rng.Perm(nSessions)
				for _, i := range order {
					uid := userIDs[rng.Intn(len(userIDs))]
					if _, err := tx.Exec(ctx,
						`UPDATE sessions SET user_id = $1, updated_at = now() WHERE id = $2`, uid, sids[i]); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				if logDeadlock(t, "reassign", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("reassign: %v", err)
					return
				}
			}
			reassigns.Add(1)
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// Settle tick: grades settled sessions in their own transactions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop() {
			if _, err := wide.RefreshSettledSignals(ctx); err != nil {
				if logDeadlock(t, "RefreshSettledSignals", err) {
					deadlocks.Add(1)
				} else {
					t.Errorf("settle: %v", err)
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	wg.Wait()
	t.Logf("stress summary: %d rebuilds, %d appends, %d announces, %d reassigns, %d pin rounds, %d sweeps, %d finalizes, %d deadlock(s)",
		rebuilds.Load(), appends.Load(), announces.Load(), reassigns.Load(), pins.Load(), sweeps.Load(), finalizes.Load(), deadlocks.Load())
}
