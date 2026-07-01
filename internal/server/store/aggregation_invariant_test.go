package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// This file pins the load-bearing invariant the whole dashboard leans on:
//
//	sessions.total_* == sum over a session's usage_events, for every token class
//	and for cost, and message_count == count of a session's messages rows.
//
// akari aggregates token and cost data from two bases. The usage_events ledger
// backs the analytics surfaces (the overview and project usage panels, the
// project sparklines); the sessions.total_* rollups back the projects index and
// every per-session figure (the session list, the session header). The two
// reconcile only because the rollups are folded from exactly the usage rows that
// survive their ON CONFLICT dedup (see applyDelta / applyAggregates), so the
// rollup equals the ledger by construction. That equality is unenforced at the
// schema level, so it is exactly the kind of thing that rots; these tests assert
// it directly, on the live ingest path and after a reparse, and check that the
// cross-base views built on top of it reconcile. See docs/data-aggregation.md for
// the full inventory of aggregation sites and which base each reads.

// usageRow is one usage_events insert for a test ingest, named so a delta reads
// like the transcript it stands in for. A zero At means an undated event (NULL
// occurred_at); a nil Cost means a model the pricing table does not know, which
// makes the session total a partial sum.
type usageRow struct {
	Model           string
	In, Out, CR, CW int64
	At              time.Time
	Cost            *float64
	DedupKey        string
	SourceOffset    int64
	SourceIndex     int
}

// fixedReduce returns a ReduceFunc that emits the same delta on every region,
// ignoring the raw bytes (the same stub shape reparse_test.go uses). A
// single-chunk session is one region, so AdvanceProjection applies the delta once
// on the live path and ReparseSession replays the identical delta on reparse.
// Usage rows dedup on their keys, so replaying the same delta is idempotent: that
// is what lets a test assert the rollups land on the same totals after ingest and
// after reparse rather than doubling.
func fixedReduce(d store.ProjectionDelta) store.ReduceFunc {
	return func(_, _ []byte, _ int64) ([]byte, store.ProjectionDelta, error) {
		return []byte("{}"), d, nil
	}
}

// ingestVersion is an arbitrary parser version for the test ingest path. The
// value does not matter to the invariant; it only has to be consistent between
// the advance and the reparse so neither trips the parser-version-stale guard.
const ingestVersion = 1

// ingestSession drives one session through the real live path (announce, append a
// raw chunk, advance the projection), so its rollups are folded by the production
// applyDelta / applyAggregates rather than hand-written. It returns the session id
// and the reducer it ingested with, so a later reparse can replay the identical
// fold. The usage rows are emitted by a fixed reducer, so the raw bytes are a
// placeholder the reducer ignores; what is exercised is the store's fold, which is
// where the invariant is forged.
func ingestSession(t *testing.T, st *store.Store, userID, projectID int64, agent, src string, msgs []store.MessageDelta, usage []usageRow) (int64, store.ReduceFunc) {
	t.Helper()
	ctx := context.Background()
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: userID, Agent: agent, SourceSessionID: src, ProjectID: projectID, Machine: "box",
	})
	if err != nil {
		t.Fatalf("announce %s: %v", src, err)
	}
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte("placeholder transcript line\n")); err != nil {
		t.Fatalf("append %s: %v", src, err)
	}
	d := store.ProjectionDelta{Messages: msgs}
	for _, u := range usage {
		d.Usage = append(d.Usage, store.ProjUsage{
			Model:        u.Model,
			Input:        int(u.In),
			Output:       int(u.Out),
			CacheRead:    int(u.CR),
			CacheWrite:   int(u.CW),
			CostUSD:      u.Cost,
			OccurredAt:   u.At,
			DedupKey:     u.DedupKey,
			SourceOffset: u.SourceOffset,
			SourceIndex:  u.SourceIndex,
		})
	}
	reduce := fixedReduce(d)
	// Loop to catch up exactly as parse.Advance does, so a session larger than one
	// batch would still be fully applied (here it is one small chunk, one region).
	for {
		_, caughtUp, err := st.AdvanceProjection(ctx, ann.SessionID, ingestVersion, reduce)
		if err != nil {
			t.Fatalf("advance %s: %v", src, err)
		}
		if caughtUp {
			break
		}
	}
	return ann.SessionID, reduce
}

// ingestOnly drives a session through the live path and drops the reducer, for
// tests that never reparse.
func ingestOnly(t *testing.T, st *store.Store, userID, projectID int64, agent, src string, msgs []store.MessageDelta, usage []usageRow) int64 {
	t.Helper()
	sid, _ := ingestSession(t, st, userID, projectID, agent, src, msgs, usage)
	return sid
}

// ledgerTotals sums a session's usage_events directly, the base the analytics
// surfaces aggregate. It is the right-hand side of the invariant.
func ledgerTotals(t *testing.T, st *store.Store, sessionID int64) (in, out, cr, cw int64, cost float64, rows int) {
	t.Helper()
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		        coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_write_tokens),0),
		        coalesce(sum(cost_usd),0), count(*)
		   FROM usage_events WHERE session_id = $1`, sessionID).
		Scan(&in, &out, &cr, &cw, &cost, &rows); err != nil {
		t.Fatalf("ledger totals for session %d: %v", sessionID, err)
	}
	return in, out, cr, cw, cost, rows
}

// rollupTotals reads a session's stored rollups, the base the projects index and
// every per-session figure read. It is the left-hand side of the invariant.
func rollupTotals(t *testing.T, st *store.Store, sessionID int64) (in, out, cr, cw int64, cost float64, msgs int) {
	t.Helper()
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT total_input_tokens, total_output_tokens, total_cache_read_tokens,
		        total_cache_write_tokens, total_cost_usd, message_count
		   FROM sessions WHERE id = $1`, sessionID).
		Scan(&in, &out, &cr, &cw, &cost, &msgs); err != nil {
		t.Fatalf("rollup totals for session %d: %v", sessionID, err)
	}
	return in, out, cr, cw, cost, msgs
}

// assertRollupMatchesLedger pins the invariant for one session: every token class
// and the cost in sessions.total_* equal the sum over its usage_events, and
// message_count equals the count of its messages rows. `when` labels the phase
// (after ingest, after reparse) so a failure says which path broke it.
func assertRollupMatchesLedger(t *testing.T, st *store.Store, sessionID int64, when string) {
	t.Helper()
	lin, lout, lcr, lcw, lcost, _ := ledgerTotals(t, st, sessionID)
	rin, rout, rcr, rcw, rcost, msgs := rollupTotals(t, st, sessionID)
	if rin != lin || rout != lout || rcr != lcr || rcw != lcw {
		t.Errorf("%s: session %d rollup tokens (in=%d out=%d cr=%d cw=%d) != ledger (in=%d out=%d cr=%d cw=%d)",
			when, sessionID, rin, rout, rcr, rcw, lin, lout, lcr, lcw)
	}
	if rcost != lcost {
		t.Errorf("%s: session %d total_cost_usd = %v != ledger cost %v", when, sessionID, rcost, lcost)
	}
	var rowCount int
	if err := st.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM messages WHERE session_id = $1", sessionID).Scan(&rowCount); err != nil {
		t.Fatalf("count messages for session %d: %v", sessionID, err)
	}
	if msgs != rowCount {
		t.Errorf("%s: session %d message_count = %d != count of messages rows %d", when, sessionID, msgs, rowCount)
	}
}

// adversarialUsage is a usage shape built to break a fold that is not careful:
// a priced cache-dominant Claude row, the SAME row repeated under a colliding
// dedup_key (Claude streams one assistant message across several lines that share
// it, so the ledger keeps one and the rollup must count one), a second priced model, an
// undated row (NULL occurred_at, which the rollup counts but the analytics time
// axis drops), and an unpriced model (tokens but no cost, which sets
// cost_incomplete). If applyDelta folded pre-dedup rows, or zeroed a subset on
// reparse, or skipped a class, the invariant assertion would catch it.
func adversarialUsage() []usageRow {
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	priced := func(v float64) *float64 { return &v }
	return []usageRow{
		{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 50000, CW: 4000, At: at, Cost: priced(1.50), DedupKey: "msg_a", SourceOffset: 10, SourceIndex: 0},
		// Claude's repeat: identical numbers, same message id, different source
		// offset. It collides on (session_id, dedup_key) and folds to one row, so the
		// rollup must not double it.
		{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 50000, CW: 4000, At: at, Cost: priced(1.50), DedupKey: "msg_a", SourceOffset: 20, SourceIndex: 0},
		{Model: "gpt-5.5", In: 800, Out: 400, CR: 0, CW: 0, At: at.Add(time.Hour), Cost: priced(0.40), DedupKey: "msg_b", SourceOffset: 30, SourceIndex: 0},
		// Undated: no occurred_at, so the analytics time axis drops it but the rollup
		// counts it. This is the one legitimate rollup/analytics gap (see
		// TestUndatedUsageIsTheOnlyRollupAnalyticsGap).
		{Model: "claude-opus-4-8", In: 100, Out: 100, CR: 0, CW: 0, Cost: priced(0.05), DedupKey: "msg_undated", SourceOffset: 40, SourceIndex: 0},
		// Unpriced model: tokens but no cost, which sets cost_incomplete and must
		// still fold its tokens into the rollup.
		{Model: "some-unpriced-model", In: 500, Out: 250, CR: 0, CW: 0, At: at.Add(2 * time.Hour), DedupKey: "msg_c", SourceOffset: 50, SourceIndex: 0},
	}
}

// TestSessionRollupMatchesLedger is the direct invariant test the audit calls for:
// for every session, sessions.total_* equals the sum over its usage_events, after
// the live ingest path and again after a reparse. It runs across several sessions,
// agents, models, cache tokens, duplicate usage, undated usage, and unpriced usage,
// so a fold that drops a class, double-counts a duplicate, or fails to zero-and-
// rebuild a class on reparse is caught.
func TestSessionRollupMatchesLedger(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projA, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project A: %v", err)
	}
	projB, err := st.UpsertProject(ctx, "github.com/hopper/compiler", "github.com", "hopper", "compiler", "compiler", "remote")
	if err != nil {
		t.Fatalf("project B: %v", err)
	}

	msgs := []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "do the thing"},
		{Ordinal: 1, Role: "assistant", Content: "done"},
	}
	type ingested struct {
		id     int64
		reduce store.ReduceFunc
	}
	var sessions []ingested
	add := func(projectID int64, agent, src string, usage []usageRow) {
		id, reduce := ingestSession(t, st, user.ID, projectID, agent, src, msgs, usage)
		sessions = append(sessions, ingested{id, reduce})
	}
	add(projA, "claude", "s-claude", adversarialUsage())
	add(projA, "codex", "s-codex", adversarialUsage())
	add(projB, "claude", "s-claude-b", adversarialUsage())
	// A session with no usage at all: the invariant must hold trivially (both sides
	// zero) rather than being skipped.
	add(projB, "claude", "s-empty", nil)

	for _, s := range sessions {
		assertRollupMatchesLedger(t, st, s.id, "after ingest")
	}

	// Reparse every session through the identical fold and re-check: the reset must
	// zero every class and the rebuild must re-accumulate to the same totals.
	for _, s := range sessions {
		if err := st.ReparseSession(ctx, s.id, ingestVersion, s.reduce); err != nil {
			t.Fatalf("reparse session %d: %v", s.id, err)
		}
		assertRollupMatchesLedger(t, st, s.id, "after reparse")
	}
}

// cacheSavingsUsage is a usage shape aimed at the savings rollup: a priced Claude row whose
// cache reads save money while its cache write costs money (the saving nets the two and can
// go negative), the SAME row repeated under a colliding dedup_key (the ledger keeps one, so
// the saving must not double), a second priced model that also carries cache reads, and an
// unpriced model that carries cache reads (its saving is omitted, so cache_savings_incomplete
// must be set while its tokens still fold). A fold that priced pre-dedup rows, dropped a
// model, missed the unpriced-with-cache case, or mishandled the negative cache-write term
// would diverge from the per-model recompute the test reconciles against.
func cacheSavingsUsage() []usageRow {
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	priced := func(v float64) *float64 { return &v }
	return []usageRow{
		{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 500_000, CW: 40_000, At: at, Cost: priced(1.50), DedupKey: "sv_a", SourceOffset: 10},
		{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 500_000, CW: 40_000, At: at, Cost: priced(1.50), DedupKey: "sv_a", SourceOffset: 20},
		{Model: "gpt-5.5", In: 800, Out: 400, CR: 100_000, CW: 0, At: at.Add(time.Hour), Cost: priced(0.40), DedupKey: "sv_b", SourceOffset: 30},
		{Model: "some-unpriced-model", In: 500, Out: 250, CR: 200_000, CW: 0, At: at.Add(2 * time.Hour), DedupKey: "sv_c", SourceOffset: 40},
	}
}

// TestCacheSavingsRollupMatchesRecompute pins the per-session cache-savings rollup
// (sessions.total_cache_savings_usd, folded per surviving usage row at parse time) against
// SessionCacheStats, the from-scratch per-model recompute over the same rows. Pricing is
// linear in tokens, so the per-row fold and the per-model recompute must land on the same
// dollars and the same incomplete flag, after the live ingest path and again after a reparse.
// It is the savings analogue of TestSessionRollupMatchesLedger: a fold that doubles a deduped
// row, drops a model, mishandles the unpriced-with-cache case, swaps the read and write terms,
// or fails to zero-and-rebuild on reparse diverges from the recompute here. The rollup is what
// the session header's Cache tile now reads in O(1), so this is the guard that the O(1) tile
// shows the same figure the per-row scan would.
func TestCacheSavingsRollupMatchesRecompute(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/savings", "github.com", "ada", "savings", "savings", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	msgs := []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "go"}}

	// reconcile pins the rollup to the independent recompute for one session, so a failure
	// says which phase (ingest or reparse) and which session broke it.
	reconcile := func(t *testing.T, id int64, when string) {
		t.Helper()
		d, err := st.SessionDetailByID(ctx, id)
		if err != nil {
			t.Fatalf("%s: session detail %d: %v", when, id, err)
		}
		recompute, err := st.SessionCacheStats(ctx, id)
		if err != nil {
			t.Fatalf("%s: session cache stats %d: %v", when, id, err)
		}
		if math.Abs(d.TotalCacheSavingsUSD-recompute.SavingsUSD) > 1e-9 {
			t.Errorf("%s: session %d rollup savings %v != recompute %v", when, id, d.TotalCacheSavingsUSD, recompute.SavingsUSD)
		}
		if d.CacheSavingsIncomplete != recompute.SavingsIncomplete {
			t.Errorf("%s: session %d rollup incomplete %v != recompute %v", when, id, d.CacheSavingsIncomplete, recompute.SavingsIncomplete)
		}
	}

	sMixed, reduceMixed := ingestSession(t, st, user.ID, proj, "claude", "s-mixed", msgs, cacheSavingsUsage())
	sEmpty, reduceEmpty := ingestSession(t, st, user.ID, proj, "claude", "s-empty", msgs, nil)

	reconcile(t, sMixed, "after ingest")
	reconcile(t, sEmpty, "after ingest")

	// Beyond reconciling with the oracle, pin the boundary values directly: the mixed session
	// saves real money from its priced models yet flags incomplete for the unpriced one, and
	// the empty session is a complete zero rather than a misleading incomplete or non-zero.
	dMixed, err := st.SessionDetailByID(ctx, sMixed)
	if err != nil {
		t.Fatalf("mixed detail: %v", err)
	}
	if !dMixed.CacheSavingsIncomplete {
		t.Error("mixed session should flag cache_savings_incomplete: an unpriced model carried cache reads")
	}
	if dMixed.TotalCacheSavingsUSD <= 0 {
		t.Errorf("mixed session should have a positive saving from its priced cache reads; got %v", dMixed.TotalCacheSavingsUSD)
	}
	dEmpty, err := st.SessionDetailByID(ctx, sEmpty)
	if err != nil {
		t.Fatalf("empty detail: %v", err)
	}
	if dEmpty.CacheSavingsIncomplete || dEmpty.TotalCacheSavingsUSD != 0 {
		t.Errorf("empty session should carry a zero complete saving; got %v incomplete=%v", dEmpty.TotalCacheSavingsUSD, dEmpty.CacheSavingsIncomplete)
	}

	// Reparse both through the identical fold: the reset must zero the saving and its flag and
	// the rebuild must re-fold to the same figure, so the rollup does not double or drift.
	for _, s := range []struct {
		id     int64
		reduce store.ReduceFunc
	}{{sMixed, reduceMixed}, {sEmpty, reduceEmpty}} {
		if err := st.ReparseSession(ctx, s.id, ingestVersion, s.reduce); err != nil {
			t.Fatalf("reparse session %d: %v", s.id, err)
		}
		reconcile(t, s.id, "after reparse")
	}
}

// TestProjectsIndexReconcilesWithAnalytics is the cross-view reconciliation the
// audit asks for: the projects index (ListProjects, rollup base) and the project
// usage panel (Analytics, ledger base) show the same project's lifetime tokens and
// cost on two different pages. With all usage dated, the two bases must agree to
// the token and the cent, or a reader gets two numbers for one project. This test
// keeps every event dated so the only remaining difference (undated usage) is held
// at zero; the undated gap itself is pinned separately below.
func TestProjectsIndexReconcilesWithAnalytics(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projA, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project A: %v", err)
	}
	projB, err := st.UpsertProject(ctx, "github.com/hopper/compiler", "github.com", "hopper", "compiler", "compiler", "remote")
	if err != nil {
		t.Fatalf("project B: %v", err)
	}

	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	priced := func(v float64) *float64 { return &v }
	// All dated, with duplicates and cache tokens, but nothing undated.
	dated := func(prefix string) []usageRow {
		return []usageRow{
			{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 50000, CW: 4000, At: at, Cost: priced(1.50), DedupKey: prefix + "-a", SourceOffset: 10},
			{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 50000, CW: 4000, At: at, Cost: priced(1.50), DedupKey: prefix + "-a", SourceOffset: 20},
			{Model: "gpt-5.5", In: 800, Out: 400, At: at.Add(time.Hour), Cost: priced(0.40), DedupKey: prefix + "-b", SourceOffset: 30},
		}
	}
	msgs := []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "hi"}}
	ingestOnly(t, st, user.ID, projA, "claude", "a1", msgs, dated("a1"))
	ingestOnly(t, st, user.ID, projA, "codex", "a2", msgs, dated("a2"))
	ingestOnly(t, st, user.ID, projB, "claude", "b1", msgs, dated("b1"))

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	byID := map[int64]store.ProjectSummary{}
	for _, p := range projects {
		byID[p.ID] = p
	}

	for _, pid := range []int64{projA, projB} {
		p, ok := byID[pid]
		if !ok {
			t.Fatalf("project %d missing from index", pid)
		}
		a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: pid, Since: time.Time{}, UserIDs: nil})
		if err != nil {
			t.Fatalf("analytics for project %d: %v", pid, err)
		}
		if p.TotalInput != a.TotalIn || p.TotalOutput != a.TotalOut ||
			p.TotalCacheRead != a.TotalCacheRead || p.TotalCacheWrite != a.TotalCacheWrite {
			t.Errorf("project %d: index rollup tokens (in=%d out=%d cr=%d cw=%d) != analytics ledger (in=%d out=%d cr=%d cw=%d)",
				pid, p.TotalInput, p.TotalOutput, p.TotalCacheRead, p.TotalCacheWrite,
				a.TotalIn, a.TotalOut, a.TotalCacheRead, a.TotalCacheWrite)
		}
		if p.TotalTokens() != a.TotalTokens() {
			t.Errorf("project %d: index TotalTokens %d != analytics TotalTokens %d", pid, p.TotalTokens(), a.TotalTokens())
		}
		if p.TotalCostUSD != a.TotalCost {
			t.Errorf("project %d: index cost %v != analytics cost %v", pid, p.TotalCostUSD, a.TotalCost)
		}
	}

	// The instance-wide overview totals must equal the sum of the per-project index
	// rollups, so the Overview page and the Projects index never disagree on the
	// fleet total.
	all, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("instance analytics: %v", err)
	}
	var sumIn, sumOut, sumCR, sumCW int64
	var sumCost float64
	for _, pid := range []int64{projA, projB} {
		p := byID[pid]
		sumIn += p.TotalInput
		sumOut += p.TotalOutput
		sumCR += p.TotalCacheRead
		sumCW += p.TotalCacheWrite
		sumCost += p.TotalCostUSD
	}
	if all.TotalIn != sumIn || all.TotalOut != sumOut || all.TotalCacheRead != sumCR || all.TotalCacheWrite != sumCW {
		t.Errorf("instance analytics tokens (in=%d out=%d cr=%d cw=%d) != sum of index rollups (in=%d out=%d cr=%d cw=%d)",
			all.TotalIn, all.TotalOut, all.TotalCacheRead, all.TotalCacheWrite, sumIn, sumOut, sumCR, sumCW)
	}
	if all.TotalCost != sumCost {
		t.Errorf("instance analytics cost %v != sum of index rollups %v", all.TotalCost, sumCost)
	}
}

// TestUndatedUsageIsTheOnlyRollupAnalyticsGap pins the single documented reason the
// rollup base and the ledger-analytics base can differ: the analytics surfaces
// filter occurred_at IS NOT NULL (an undated event has no day to plot), while the
// rollups count every surviving usage row. So an undated event raises the projects
// index (rollup) figure above the all-time project usage panel (analytics) figure
// by exactly its own tokens and cost, and nothing else. Pinning the gap to exactly
// the undated amount turns any OTHER divergence (a dropped class, a stray filter)
// into a test failure.
func TestUndatedUsageIsTheOnlyRollupAnalyticsGap(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	priced := func(v float64) *float64 { return &v }
	const undatedIn, undatedOut, undatedCR, undatedCW = 100, 200, 300, 400
	undatedCost := 0.25
	usage := []usageRow{
		{Model: "claude-opus-4-8", In: 1000, Out: 2000, CR: 50000, CW: 4000, At: at, Cost: priced(1.50), DedupKey: "dated", SourceOffset: 10},
		{Model: "claude-opus-4-8", In: undatedIn, Out: undatedOut, CR: undatedCR, CW: undatedCW, Cost: priced(undatedCost), DedupKey: "undated", SourceOffset: 20},
	}
	sid := ingestOnly(t, st, user.ID, proj, "claude", "s1", []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "hi"}}, usage)

	// The rollup counts the undated event; assert it so the gap below is measured
	// against a rollup that genuinely includes it.
	assertRollupMatchesLedger(t, st, sid, "after ingest")

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	var p store.ProjectSummary
	for _, pr := range projects {
		if pr.ID == proj {
			p = pr
		}
	}
	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}

	if got := p.TotalInput - a.TotalIn; got != undatedIn {
		t.Errorf("input gap = %d, want exactly the undated %d", got, undatedIn)
	}
	if got := p.TotalOutput - a.TotalOut; got != undatedOut {
		t.Errorf("output gap = %d, want exactly the undated %d", got, undatedOut)
	}
	if got := p.TotalCacheRead - a.TotalCacheRead; got != undatedCR {
		t.Errorf("cache-read gap = %d, want exactly the undated %d", got, undatedCR)
	}
	if got := p.TotalCacheWrite - a.TotalCacheWrite; got != undatedCW {
		t.Errorf("cache-write gap = %d, want exactly the undated %d", got, undatedCW)
	}
	if got := p.TotalCostUSD - a.TotalCost; got < undatedCost-1e-9 || got > undatedCost+1e-9 {
		t.Errorf("cost gap = %v, want exactly the undated %v", got, undatedCost)
	}
}

// TestCostIncompleteFollowsUndatedGap pins that the cost-incomplete marker rides the
// same documented rollup-vs-analytics gap as the token and cost totals. An undated,
// unpriced usage row (tokens, no cost, no occurred_at) sets the session rollup's
// flag, so the projects index reads "$X+", but it is dropped from the dated
// analytics base, so the all-time panel's cost is exact and reads no marker. The two
// surfaces are allowed to differ here, and only here, for exactly the undated row;
// any OTHER source of divergence in the flag would be a bug this guards against.
func TestCostIncompleteFollowsUndatedGap(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// One undated, unpriced row: tokens but no cost (unpriced model) and no
	// occurred_at (no place on the time axis).
	usage := []usageRow{
		{Model: "some-unpriced-model", In: 500, Out: 250, DedupKey: "undated-unpriced", SourceOffset: 10},
	}
	sid := ingestOnly(t, st, user.ID, proj, "claude", "s1", []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "hi"}}, usage)
	assertRollupMatchesLedger(t, st, sid, "after ingest")

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	var p store.ProjectSummary
	for _, pr := range projects {
		if pr.ID == proj {
			p = pr
		}
	}
	if !p.CostIncomplete {
		t.Error("projects index should flag the project cost-incomplete: the rollup counts the undated unpriced row")
	}

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if a.CostIncomplete {
		t.Error("all-time analytics drops the undated row, so its cost is exact and must not flag incomplete")
	}
}
