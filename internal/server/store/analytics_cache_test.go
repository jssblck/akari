package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestCacheStats pins the scoped cache aggregate: the prompt-token split, the hit rate
// (cache reads over all prompt tokens), and the dollars saved (the input-versus-cache
// rate gap, priced per model). Two Opus sessions with known cache volume let every
// figure be asserted against a hand-computed value.
func TestCacheStats(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/cache", "github.com", "ada", "cache", "cache", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Session A: 200k uncached input, 800k cached reads (0.8 hit rate alone), no writes.
	// Saving = 0.8M * (5 - 0.50) = 3.60.
	sA := seedSessionWithStats(t, st, admin.ID, proj, "claude", "a", 1, 200_000, 100_000)
	seedUsageCache(t, st, sA, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 0, "a-1")
	// Session B: 500k input, 500k cached reads (0.5 hit rate alone).
	// Saving = 0.5M * (5 - 0.50) = 2.25.
	sB := seedSessionWithStats(t, st, admin.ID, proj, "claude", "b", 1, 500_000, 100_000)
	seedUsageCache(t, st, sB, "claude-opus-4-8", 1, 500_000, 100_000, 500_000, 0, 0, "b-1")

	c, err := st.CacheStats(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("cache stats: %v", err)
	}
	// Combined: input 700k, cache read 1.3M, prompt tokens 2.0M, hit rate 0.65.
	if c.Input != 700_000 || c.CacheRead != 1_300_000 || c.CacheWrite != 0 {
		t.Errorf("token split = in %d / read %d / write %d, want 700000 / 1300000 / 0", c.Input, c.CacheRead, c.CacheWrite)
	}
	if c.PromptTokens() != 2_000_000 {
		t.Errorf("prompt tokens = %d, want 2000000", c.PromptTokens())
	}
	if math.Abs(c.HitRate()-0.65) > 1e-9 {
		t.Errorf("hit rate = %v, want 0.65", c.HitRate())
	}
	if math.Abs(c.SavingsUSD-5.85) > 1e-9 {
		t.Errorf("savings = %v, want 5.85 (3.60 + 2.25)", c.SavingsUSD)
	}
	if c.SavingsIncomplete {
		t.Error("savings should be complete: every model is priced")
	}
}

// TestCacheStatsReconcilesWithSnapshotTotals pins the Cache tile to the token totals under
// the snapshot path. AnalyticsSnapshot reads the whole aggregate from one repeatable-read
// transaction, and the cache aggregate now threads that same transaction rather than a
// second pooled connection, so the Cache tile's prompt-token sums must equal the headline
// token classes drawn from the same dated usage. If the cache tile read a different snapshot
// or a different base, one of these equalities would break.
func TestCacheStatsReconcilesWithSnapshotTotals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/recon", "github.com", "ada", "recon", "recon", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Two agents, two models, all dated, with input, output, cache read, and cache write
	// volume so every token class is non-zero on both sides of the reconciliation.
	sA := seedSessionWithStats(t, st, admin.ID, proj, "claude", "a", 0, 0, 0)
	seedUsageCache(t, st, sA, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 50_000, 1, "a-1")
	sB := seedSessionWithStats(t, st, admin.ID, proj, "codex", "b", 0, 0, 0)
	seedUsageCache(t, st, sB, "gpt-5.5", 1, 500_000, 300_000, 500_000, 20_000, 1, "b-1")

	a, ok, err := st.AnalyticsSnapshot(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics snapshot: %v", err)
	}
	if !ok {
		t.Fatal("snapshot returned ok=false with no reparse in flight")
	}

	if a.Cache.Input != a.TotalIn {
		t.Errorf("cache input %d != total input %d", a.Cache.Input, a.TotalIn)
	}
	if a.Cache.Output != a.TotalOut {
		t.Errorf("cache output %d != total output %d", a.Cache.Output, a.TotalOut)
	}
	if a.Cache.CacheRead != a.TotalCacheRead {
		t.Errorf("cache read %d != total cache read %d", a.Cache.CacheRead, a.TotalCacheRead)
	}
	if a.Cache.CacheWrite != a.TotalCacheWrite {
		t.Errorf("cache write %d != total cache write %d", a.Cache.CacheWrite, a.TotalCacheWrite)
	}
	// Guard against a vacuous 0 == 0: confirm the volume actually landed.
	if a.TotalIn != 700_000 || a.TotalCacheRead != 1_300_000 || a.TotalCacheWrite != 70_000 {
		t.Errorf("unexpected totals: in %d, read %d, write %d (want 700000 / 1300000 / 70000)",
			a.TotalIn, a.TotalCacheRead, a.TotalCacheWrite)
	}
}

// TestCacheStatsIncompleteAndUndated pins two boundaries: an unpriced model's cached
// volume flags the saving incomplete (a lower bound) rather than dropping silently to
// zero, and the scoped analytics path excludes undated usage (matching the panel's time
// axis) while the per-session path counts it (matching the session's token rollups).
func TestCacheStatsIncompleteAndUndated(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/mix", "github.com", "ada", "mix", "mix", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s", 1, 0, 0)
	// A priced dated event and an unpriced dated event that carries cached reads: the
	// scoped saving must flag incomplete because the unpriced model's saving is omitted.
	seedUsageCache(t, st, s, "claude-opus-4-8", 1, 100_000, 50_000, 100_000, 0, 0, "priced")
	seedUsageCacheUndatedOrUnpriced(t, st, s, "secret-model", 0, 0, 200_000, 0, true, "unpriced")

	c, err := st.CacheStats(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("cache stats: %v", err)
	}
	if !c.SavingsIncomplete {
		t.Error("savings should be incomplete: an unpriced model carried cached reads")
	}

	// Add an UNDATED cached event. The scoped (dated) path must not see it; the
	// per-session path must, since the session's token rollups count it.
	seedUsageCacheUndatedOrUnpriced(t, st, s, "claude-opus-4-8", 0, 0, 1_000_000, 0, false, "undated")

	scoped, err := st.CacheStats(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("scoped cache stats: %v", err)
	}
	// Scoped cache read is the priced dated event's reads only (100k); the undated
	// million and the unpriced event's reads carry no dated, priced read contribution
	// the panel would plot. (The unpriced dated event's 200k reads still count in the
	// token split, only its saving is omitted.)
	if scoped.CacheRead != 300_000 {
		t.Errorf("scoped cache read = %d, want 300000 (100k priced + 200k unpriced, both dated; the undated 1M excluded)", scoped.CacheRead)
	}

	sess, err := st.SessionCacheStats(ctx, s)
	if err != nil {
		t.Fatalf("session cache stats: %v", err)
	}
	// Per-session counts every row: 100k + 200k + 1M = 1.3M cached reads.
	if sess.CacheRead != 1_300_000 {
		t.Errorf("session cache read = %d, want 1300000 (counts the undated event too)", sess.CacheRead)
	}
}

// markNeedsCacheBackfill flags a session as not-yet-backfilled, standing in for a session that
// predates the rollup column (the migration marks such cache-bearing sessions false). A test
// session is ingested after the column, so it defaults authoritative (backfilled=true); a backfill
// test that wants it treated as a candidate clears the flag here.
func markNeedsCacheBackfill(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET cache_savings_backfilled = false WHERE id = $1", sid); err != nil {
		t.Fatalf("mark session %d needs cache backfill: %v", sid, err)
	}
}

// storedCacheSavings reads the persisted rollup columns directly, bypassing the scanDetail
// read-side gate. A test uses it to assert what is stored on the session row, as opposed to the
// value the Cache tile would compute live for an unbackfilled session.
func storedCacheSavings(t *testing.T, st *store.Store, ctx context.Context, sid int64) (float64, bool) {
	t.Helper()
	var v float64
	var incomplete bool
	if err := st.Pool.QueryRow(ctx,
		"SELECT total_cache_savings_usd, cache_savings_incomplete FROM sessions WHERE id = $1", sid).
		Scan(&v, &incomplete); err != nil {
		t.Fatalf("read stored cache savings for %d: %v", sid, err)
	}
	return v, incomplete
}

// TestSessionDetailBackfillsUnbackfilledCacheSavingsOnRead pins the read-side cache-savings gate. A
// session whose rollup is not yet backfilled (the migration seeds it at 0 and the async backfill has
// not reached it) must not have the seeded value served: the detail read prices the saving from
// usage_events once, persists it, and flips the flag, so the tile shows the real figure and every
// later read is the O(1) stored rollup rather than a per-refresh rescan. This is the read-side
// companion to BackfillCacheSavings.
func TestSessionDetailBackfillsUnbackfilledCacheSavingsOnRead(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/readgate", "github.com", "ada", "readgate", "readgate", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "readgate", 0, 0, 0)
	seedUsageCache(t, st, s, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 1, "rg-1")
	recompute, err := st.SessionCacheStats(ctx, s)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recompute.SavingsUSD <= 0 || recompute.SavingsIncomplete {
		t.Fatalf("test needs a positive, complete recompute saving, got %v incomplete=%v", recompute.SavingsUSD, recompute.SavingsIncomplete)
	}

	// Unbackfilled, with a deliberately wrong stored rollup and stale incomplete flag, standing in
	// for a pre-0020 session the startup backfill has not yet reached.
	markNeedsCacheBackfill(t, st, ctx, s)
	const wrong = 99.0
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET total_cache_savings_usd = $2, cache_savings_incomplete = true WHERE id = $1", s, wrong); err != nil {
		t.Fatalf("seed wrong rollup: %v", err)
	}

	// The read serves the priced saving, not the seeded one.
	d, err := st.SessionDetailByID(ctx, s)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if math.Abs(d.TotalCacheSavingsUSD-recompute.SavingsUSD) > 1e-9 {
		t.Errorf("read savings = %v, want the priced recompute %v (not the seeded %v)", d.TotalCacheSavingsUSD, recompute.SavingsUSD, wrong)
	}
	if d.CacheSavingsIncomplete {
		t.Error("read should have priced a complete saving, not carried the seeded incomplete=true")
	}

	// The read persisted the saving and flipped the flag, so the row is now authoritative and a
	// later read is the O(1) rollup rather than another usage_events scan.
	stored, incomplete := storedCacheSavings(t, st, ctx, s)
	if math.Abs(stored-recompute.SavingsUSD) > 1e-9 || incomplete {
		t.Errorf("stored rollup after read = %v incomplete=%v, want the priced %v / false persisted", stored, incomplete, recompute.SavingsUSD)
	}
	var backfilled bool
	if err := st.Pool.QueryRow(ctx,
		"SELECT cache_savings_backfilled FROM sessions WHERE id = $1", s).Scan(&backfilled); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	if !backfilled {
		t.Error("read should have flipped cache_savings_backfilled so later reads skip the recompute")
	}
}

// TestBackfillCacheSavings pins the startup backfill that repairs a session whose parse-time
// savings fold never ran: one ingested before the rollup column existed, or one whose reparse
// deterministically fails so the epoch reparse cannot fill it. The saving is a pure function of
// usage_events, so the backfill prices the ledger directly and lands on the same figure the
// per-model recompute does. It is non-parallel because it shrinks the batch global to exercise
// the multi-batch keyset drain across two candidate sessions.
func TestBackfillCacheSavings(t *testing.T) {
	// Batch of one, so two candidates span two internal batches and the keyset cursor has to
	// advance past each priced session. Not parallel: it mutates a package global.
	defer store.SetCacheSavingsBackfillBatch(1)()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/backfill", "github.com", "ada", "backfill", "backfill", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Priced cache reads whose rollup sits at the column default (the pre-column / failed-parse
	// state): the backfill must price it to a real, positive saving.
	sPriced := seedSessionWithStats(t, st, admin.ID, proj, "claude", "priced", 0, 0, 0)
	seedUsageCache(t, st, sPriced, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 1, "p-1")
	// Cache volume on an unpriced model: the backfill must flag the saving incomplete rather
	// than leave a clean zero that reads as "no saving".
	sUnpriced := seedSessionWithStats(t, st, admin.ID, proj, "claude", "unpriced", 0, 0, 0)
	seedUsageCacheUndatedOrUnpriced(t, st, sUnpriced, "secret-model", 100_000, 0, 300_000, 0, true, "u-1")
	// No cache tokens: not a candidate even when flagged for backfill, so it must be left
	// untouched (the EXISTS probe excludes it).
	sNoCache := seedSessionWithStats(t, st, admin.ID, proj, "claude", "nocache", 0, 0, 0)
	seedUsageCache(t, st, sNoCache, "claude-opus-4-8", 1, 50_000, 20_000, 0, 0, 1, "n-1")

	// All three predate the column (flag cleared); only the two cache-bearing ones are candidates.
	for _, id := range []int64{sPriced, sUnpriced, sNoCache} {
		markNeedsCacheBackfill(t, st, ctx, id)
	}

	// The persisted rollup is at the column default before the backfill prices it. Read the
	// column directly, not through SessionDetailByID: the read-side gate recomputes an
	// unbackfilled session's saving live (see TestSessionDetailRecomputesUnbackfilledCacheSavings),
	// so the tile would already show a nonzero figure; here we are pinning the stored state.
	for _, id := range []int64{sPriced, sUnpriced, sNoCache} {
		if v, incomplete := storedCacheSavings(t, st, ctx, id); v != 0 || incomplete {
			t.Fatalf("session %d pre-backfill stored rollup = %v incomplete=%v, want zero/false", id, v, incomplete)
		}
	}

	n, err := st.BackfillCacheSavings(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d sessions, want 2 (the two cache-carrying ones; the no-cache session is not a candidate)", n)
	}

	// The priced session now carries the same figure a from-scratch per-model recompute gives,
	// and it is positive and complete.
	recompute, err := st.SessionCacheStats(ctx, sPriced)
	if err != nil {
		t.Fatalf("recompute priced: %v", err)
	}
	dp, err := st.SessionDetailByID(ctx, sPriced)
	if err != nil {
		t.Fatalf("priced detail: %v", err)
	}
	if math.Abs(dp.TotalCacheSavingsUSD-recompute.SavingsUSD) > 1e-9 {
		t.Errorf("priced savings %v != recompute %v", dp.TotalCacheSavingsUSD, recompute.SavingsUSD)
	}
	if dp.TotalCacheSavingsUSD <= 0 || dp.CacheSavingsIncomplete {
		t.Errorf("priced session savings = %v incomplete=%v, want positive and complete", dp.TotalCacheSavingsUSD, dp.CacheSavingsIncomplete)
	}

	// The unpriced session is flagged incomplete (its saving is omitted), not a clean zero.
	du, err := st.SessionDetailByID(ctx, sUnpriced)
	if err != nil {
		t.Fatalf("unpriced detail: %v", err)
	}
	if !du.CacheSavingsIncomplete {
		t.Error("unpriced-cache session should be flagged incomplete after backfill")
	}

	// The no-cache session was never a backfill candidate (the EXISTS probe excludes it), so its
	// saving stays 0 whether read before or after the detail read's on-demand backfill prices its
	// empty cache to 0.
	dn, err := st.SessionDetailByID(ctx, sNoCache)
	if err != nil {
		t.Fatalf("nocache detail: %v", err)
	}
	if dn.TotalCacheSavingsUSD != 0 || dn.CacheSavingsIncomplete {
		t.Errorf("no-cache session shows a saving: %v incomplete=%v", dn.TotalCacheSavingsUSD, dn.CacheSavingsIncomplete)
	}

	// Idempotent: both priced sessions are now marked cache_savings_backfilled, so a second pass
	// finds no candidates and corrects nothing.
	n2, err := st.BackfillCacheSavings(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second backfill corrected %d, want 0 (idempotent)", n2)
	}
}

// TestBackfillCacheSavingsSkipsAlreadyBackfilled pins the row-lock recheck: a session already
// marked cache_savings_backfilled (the default for a session ingested after the column, or one a
// concurrent backfill just priced) is left untouched even when it carries cache tokens. The
// recheck keys on the flag, not the stored number, so an authoritative session is never re-priced
// and a concurrent live fold can never be clobbered.
func TestBackfillCacheSavingsSkipsAlreadyBackfilled(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/recheck", "github.com", "ada", "recheck", "recheck", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// A cache-carrying session left at the authoritative default (backfilled=true) with a stored
	// figure, standing in for a session already priced by its live fold.
	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "recheck", 0, 0, 0)
	seedUsageCache(t, st, s, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 1, "r-1")
	const sentinel = 4.2
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET total_cache_savings_usd = $2 WHERE id = $1", s, sentinel); err != nil {
		t.Fatalf("seed priced rollup: %v", err)
	}

	wrote, err := st.BackfillOneCacheSavings(ctx, s)
	if err != nil {
		t.Fatalf("backfill one: %v", err)
	}
	if wrote {
		t.Error("backfill re-priced an already-backfilled session; the recheck should have skipped it")
	}
	d, err := st.SessionDetailByID(ctx, s)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if math.Abs(d.TotalCacheSavingsUSD-sentinel) > 1e-9 {
		t.Errorf("rollup = %v, want %v preserved (an authoritative session is not re-priced)", d.TotalCacheSavingsUSD, sentinel)
	}
}

// TestBackfillCacheSavingsRepairsPartialFold pins the reason the backfill keys on the flag rather
// than a zero rollup: a session seeded at 0 that took a live append carries a partial nonzero
// total, and the backfill must recompute the full value, not skip it as "already priced". A
// candidate (backfilled=false) holding a deliberately-wrong partial figure is repaired to the full
// per-model recompute. The old "total is zero" candidate test would have skipped this.
func TestBackfillCacheSavingsRepairsPartialFold(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/partial", "github.com", "ada", "partial", "partial", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "partial", 0, 0, 0)
	seedUsageCache(t, st, s, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 1, "p-1")
	markNeedsCacheBackfill(t, st, ctx, s)
	// A partial fold: a nonzero figure that is not the full recompute, the state a post-migration
	// append on a mis-seeded base leaves behind.
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET total_cache_savings_usd = 0.01 WHERE id = $1", s); err != nil {
		t.Fatalf("seed partial fold: %v", err)
	}

	recompute, err := st.SessionCacheStats(ctx, s)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	wrote, err := st.BackfillOneCacheSavings(ctx, s)
	if err != nil {
		t.Fatalf("backfill one: %v", err)
	}
	if !wrote {
		t.Fatal("backfill skipped a partial-fold candidate; it must recompute the full value")
	}
	d, err := st.SessionDetailByID(ctx, s)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if math.Abs(d.TotalCacheSavingsUSD-recompute.SavingsUSD) > 1e-9 {
		t.Errorf("repaired rollup = %v, want the full recompute %v, not the 0.01 partial", d.TotalCacheSavingsUSD, recompute.SavingsUSD)
	}
	if d.TotalCacheSavingsUSD <= 0.01 {
		t.Errorf("repaired rollup = %v, want it replaced by the larger full value, not left at the partial", d.TotalCacheSavingsUSD)
	}
}

// setCacheSavingsPricedVersion overwrites the parse_meta pricing marker, standing in for the corpus
// having last been priced under an earlier pricing.Version so the next BackfillCacheSavings runs the
// reprice reconcile. readCacheSavingsPricedVersion reads it back to assert the reconcile advanced it.
func setCacheSavingsPricedVersion(t *testing.T, st *store.Store, ctx context.Context, v int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		"UPDATE parse_meta SET cache_savings_priced_version = $1 WHERE id = TRUE", v); err != nil {
		t.Fatalf("set cache_savings_priced_version: %v", err)
	}
}

func readCacheSavingsPricedVersion(t *testing.T, st *store.Store, ctx context.Context) int {
	t.Helper()
	var v int
	if err := st.Pool.QueryRow(ctx,
		"SELECT cache_savings_priced_version FROM parse_meta WHERE id = TRUE").Scan(&v); err != nil {
		t.Fatalf("read cache_savings_priced_version: %v", err)
	}
	return v
}

// backfilledFlag reads a session's cache_savings_backfilled directly, so a test can assert the reprice
// reconcile flipped an authoritative session back into the candidate set and the backfill flipped it
// forward again.
func backfilledFlag(t *testing.T, st *store.Store, ctx context.Context, sid int64) bool {
	t.Helper()
	var f bool
	if err := st.Pool.QueryRow(ctx,
		"SELECT cache_savings_backfilled FROM sessions WHERE id = $1", sid).Scan(&f); err != nil {
		t.Fatalf("read cache_savings_backfilled for %d: %v", sid, err)
	}
	return f
}

// TestCacheSavingsRepriceReconcile pins the pricing-version reconcile that closes the failed-reparse
// gap. A rate change re-prices the whole cache-bearing corpus, but a session whose reparse fails keeps
// its old-priced rollup with cache_savings_backfilled=true, so the ordinary backfill (which only
// visits backfilled=false candidates) skips it and its tile drifts from a live recompute forever.
// reconcileCacheSavingsPricingIfNeeded marks such sessions for re-price when pricing.Version moves past
// the stored marker, in a statement committed independently of any reparse, so even a session that
// never re-parses is corrected. This test stands in for that session: an authoritative rollup carrying
// a stale-priced (deliberately wrong) figure, re-priced to the live recompute by the reconcile, and
// then left alone once the marker is current so a steady-state startup never re-prices.
func TestCacheSavingsRepriceReconcile(t *testing.T) {
	// Not parallel: it writes the singleton parse_meta pricing marker, shared process-wide state on
	// this store's database.
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/reprice", "github.com", "ada", "reprice", "reprice", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// A cache-bearing session already priced and authoritative (backfilled=true), the state a
	// successfully-folded session sits in. Its stored rollup is then overwritten with a deliberately
	// wrong figure, standing in for a session left at the old rates by a reparse that rolled back after
	// a reprice: the value is stale but the flag says "done", so the plain backfill would never revisit it.
	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "reprice", 0, 0, 0)
	seedUsageCache(t, st, s, "claude-opus-4-8", 1, 200_000, 100_000, 800_000, 0, 1, "rp-1")
	recompute, err := st.SessionCacheStats(ctx, s)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recompute.SavingsUSD <= 0 || recompute.SavingsIncomplete {
		t.Fatalf("test needs a positive, complete recompute saving, got %v incomplete=%v", recompute.SavingsUSD, recompute.SavingsIncomplete)
	}
	const stalePriced = 1.0 // a wrong figure, as if priced under superseded rates
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET total_cache_savings_usd = $2, cache_savings_backfilled = true WHERE id = $1", s, stalePriced); err != nil {
		t.Fatalf("seed stale-priced authoritative rollup: %v", err)
	}
	if math.Abs(stalePriced-recompute.SavingsUSD) < 1e-6 {
		t.Fatalf("test setup is vacuous: stale figure %v equals the recompute %v", stalePriced, recompute.SavingsUSD)
	}

	// Rewind the pricing marker behind pricing.Version, standing in for a rate change the running
	// binary carries. The next backfill must run the reconcile, flip this authoritative session back
	// into the candidate set, and re-price it to the live recompute.
	setCacheSavingsPricedVersion(t, st, ctx, pricing.Version-1)
	if _, err := st.BackfillCacheSavings(ctx); err != nil {
		t.Fatalf("backfill after reprice: %v", err)
	}
	stored, incomplete := storedCacheSavings(t, st, ctx, s)
	if math.Abs(stored-recompute.SavingsUSD) > 1e-9 || incomplete {
		t.Errorf("re-priced rollup = %v incomplete=%v, want the live recompute %v / false (not the stale %v)", stored, incomplete, recompute.SavingsUSD, stalePriced)
	}
	if !backfilledFlag(t, st, ctx, s) {
		t.Error("the backfill should have re-set cache_savings_backfilled after re-pricing")
	}
	if v := readCacheSavingsPricedVersion(t, st, ctx); v != pricing.Version {
		t.Errorf("pricing marker = %d after reconcile, want %d", v, pricing.Version)
	}

	// Steady state: the marker is now current, so a later startup must NOT re-price. Poke another
	// wrong figure onto the authoritative row and confirm a second backfill leaves it untouched, so the
	// reconcile is genuinely gated to once per pricing change and never re-prices an authoritative row.
	const wrongAgain = 2.0
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET total_cache_savings_usd = $2 WHERE id = $1", s, wrongAgain); err != nil {
		t.Fatalf("re-seed wrong rollup: %v", err)
	}
	n, err := st.BackfillCacheSavings(ctx)
	if err != nil {
		t.Fatalf("steady-state backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("steady-state backfill corrected %d sessions, want 0 (marker current, nothing to re-price)", n)
	}
	if stored, _ := storedCacheSavings(t, st, ctx, s); math.Abs(stored-wrongAgain) > 1e-9 {
		t.Errorf("steady-state backfill changed an authoritative rollup to %v, want the poked %v left untouched", stored, wrongAgain)
	}
}

// seedUsageCacheUndatedOrUnpriced inserts a usage event carrying cache tokens that is
// either undated (no occurred_at, so the scoped analytics path drops it) or dated, and
// either unpriced (NULL cost, so it flags the saving incomplete) or priced. It covers
// the two boundary shapes the dated cache tests need without widening the shared
// seedUsageCache helper.
func seedUsageCacheUndatedOrUnpriced(t *testing.T, st *store.Store, sessionID int64, model string, in, out, cacheRead, cacheWrite int64, dated bool, dedup string) {
	t.Helper()
	occurred := "NULL"
	if dated {
		occurred = "now()"
	}
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4,$5,$6, NULL, `+occurred+`, $7)`,
		sessionID, model, in, out, cacheRead, cacheWrite, dedup)
	if err != nil {
		t.Fatalf("seed cache usage: %v", err)
	}
}
