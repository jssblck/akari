package store_test

import (
	"context"
	"math"
	"testing"

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
