package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedMessage inserts one transcript message under a session and keeps the
// session's message_count rollup in step (the ingest path maintains it; a direct
// seed must, or the default empty-hide filter would drop the session). ordinal
// orders the message within its session.
func seedMessage(t *testing.T, st *store.Store, sessionID int64, ordinal int, role, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1,$2,$3,$4)`,
		sessionID, ordinal, role, content); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET message_count = message_count + 1,
		   user_message_count = user_message_count + (CASE WHEN $2 = 'user' THEN 1 ELSE 0 END)
		 WHERE id = $1`, sessionID, role); err != nil {
		t.Fatalf("bump message count: %v", err)
	}
}

// TestListAllSessionsSearch exercises the content search: it narrows to sessions
// with a matching message, carries a windowed snippet with the match offsets, and
// treats the LIKE metacharacters (% and _) as literals so a query containing one
// matches itself rather than acting as a wildcard.
func TestListAllSessionsSearch(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	hit := seedSess(t, st, u.ID, proj, "claude", "box", "hit")
	miss := seedSess(t, st, u.ID, proj, "claude", "box", "miss")
	literal := seedSess(t, st, u.ID, proj, "claude", "box", "literal")

	seedMessage(t, st, hit, 0, "user", "Please refactor the pricing reconcile pass so it re-prices cleanly.")
	seedMessage(t, st, miss, 0, "user", "Something entirely unrelated about the weather.")
	// A message whose content contains a literal percent and underscore, so a query
	// carrying those characters must match this row and not everything.
	seedMessage(t, st, literal, 0, "user", "The cache hit rate is 95% for cache_read tokens here.")

	// A plain term narrows to the one matching session and carries a snippet whose
	// offsets bracket the match (case-insensitive).
	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "PRICING"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != hit {
		t.Fatalf("search 'pricing' = %d rows (want 1, session %d): %+v", len(rows), hit, rows)
	}
	snip := rows[0].Search
	if !snip.Has() {
		t.Fatal("matching row should carry a snippet")
	}
	if got := strings.ToLower(snip.Text[snip.MatchStart:snip.MatchEnd]); got != "pricing" {
		t.Errorf("snippet match run = %q, want the matched term 'pricing'", got)
	}

	// The '%' is a literal, not a wildcard: "95%" matches only the literal-bearing
	// session, and a bare "%" does not match every session.
	pct, _, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "95%"})
	if err != nil {
		t.Fatalf("search '95%%': %v", err)
	}
	if len(pct) != 1 || pct[0].ID != literal {
		t.Fatalf("search '95%%' = %d rows (want 1, session %d)", len(pct), literal)
	}
	// A bare '%' is a literal percent, not the match-anything wildcard: it matches
	// only the one session whose content contains a literal '%' (the "95%" row), not
	// every session. This is the escaping working: an unescaped '%' would match all.
	bare, _, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "%"})
	if err != nil {
		t.Fatalf("search bare '%%': %v", err)
	}
	if len(bare) != 1 || bare[0].ID != literal {
		t.Errorf("a bare '%%' should match only the literal-percent session, got %d rows", len(bare))
	}

	// The '_' is a literal too: "cache_read" matches only the literal session, and
	// it must not behave like the single-character wildcard "cache?read".
	under, _, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "cache_read"})
	if err != nil {
		t.Fatalf("search 'cache_read': %v", err)
	}
	if len(under) != 1 || under[0].ID != literal {
		t.Fatalf("search 'cache_read' = %d rows (want 1, session %d)", len(under), literal)
	}
}

// TestListAllSessionsSearchBoundsWindow asserts the search LATERAL bounds the content
// it pulls back per row: a message far larger than the snippet window still yields a
// correct snippet around a match deep inside it, and the snippet is built from the
// bounded SQL window rather than the whole (multi-kilobyte) message. It exercises the
// streaming-memory fix end to end: strpos locates the match, the substring windows
// around it, and the front-cut flag drives the leading ellipsis.
func TestListAllSessionsSearchBoundsWindow(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// A message far larger than the SQL window, with the match buried deep in the
	// middle so both sides are cut: ~10 KB of filler, the needle, then ~10 KB more.
	big := seedSess(t, st, u.ID, proj, "claude", "box", "big")
	lead := strings.Repeat("alpha bravo charlie delta ", 400)
	trail := strings.Repeat("echo foxtrot golf hotel ", 400)
	seedMessage(t, st, big, 0, "user", lead+"UNIQUENEEDLE "+trail)

	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "uniqueneedle"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != big {
		t.Fatalf("search = %d rows, want the one big session %d", len(rows), big)
	}
	snip := rows[0].Search
	if !snip.Has() {
		t.Fatal("the big-message match should carry a snippet")
	}
	if got := strings.ToLower(snip.Text[snip.MatchStart:snip.MatchEnd]); got != "uniqueneedle" {
		t.Errorf("snippet match run = %q, want the buried needle", got)
	}
	// The snippet is bounded near the display window, not the ~20 KB message: the SQL
	// window plus Go's trim keep peak work proportional to the snippet, not the corpus.
	// The display window is ~160 chars; 256 bytes is a generous ceiling that a whole
	// 20 KB message would blow past by orders of magnitude, so it pins the bounding.
	const snippetCeiling = 256
	if len(snip.Text) > snippetCeiling {
		t.Errorf("snippet = %d bytes, want bounded near the display window (message was ~20 KB)", len(snip.Text))
	}
	// A match this deep is front-cut by the SQL window, so the snippet leads with an
	// ellipsis rather than reading as the message's start.
	if !strings.HasPrefix(snip.Text, "…") {
		t.Errorf("a buried match should lead with an ellipsis (front-cut), got %q", snip.Text)
	}
}

// TestListAllSessionsHasMore asserts the limit+1 probe reports a further page without a
// separate count: with more rows than the limit, the list returns exactly limit rows and
// hasMore true; when the page holds the whole set, hasMore is false.
func TestListAllSessionsHasMore(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	for i := 0; i < 5; i++ {
		s := seedEmptySess(t, st, u.ID, proj, "hm"+string(rune('a'+i)))
		seedMessage(t, st, s, 0, "user", "hello")
	}

	// Limit 3 of 5: exactly 3 rows and hasMore true.
	page, more, err := st.ListAllSessions(ctx, store.SessionFilter{Limit: 3})
	if err != nil {
		t.Fatalf("list limit 3: %v", err)
	}
	if len(page) != 3 || !more {
		t.Errorf("limit 3 of 5: rows=%d hasMore=%v, want 3 and true", len(page), more)
	}

	// Limit 5 of 5: all rows, hasMore false (the page is the whole set).
	all, more, err := st.ListAllSessions(ctx, store.SessionFilter{Limit: 5})
	if err != nil {
		t.Fatalf("list limit 5: %v", err)
	}
	if len(all) != 5 || more {
		t.Errorf("limit 5 of 5: rows=%d hasMore=%v, want 5 and false", len(all), more)
	}
}

// TestHasEmptySessions asserts the bounded empty-probe: it reports true when a
// zero-message session sits in the filter's scope and false when none does, regardless
// of the current IncludeEmpty toggle (it forces empties into scope to answer "are there
// any", which is what the footer's toggle needs).
func TestHasEmptySessions(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	full := seedEmptySess(t, st, u.ID, proj, "he-full")
	seedMessage(t, st, full, 0, "user", "hi")

	// No empty yet: only the full session exists.
	has, err := st.HasEmptySessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if has {
		t.Error("no empty session should report false")
	}

	// Add an empty session: the probe now finds it, even though the default filter
	// hides empties (the probe forces them into scope to answer the yes/no).
	seedEmptySess(t, st, u.ID, proj, "he-empty")
	has, err = st.HasEmptySessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("probe after empty: %v", err)
	}
	if !has {
		t.Error("an empty session in scope should report true")
	}

	// A filter that excludes the empty (a different agent) reports false: the probe
	// respects the other conditions, so the toggle only appears when it would change
	// the current scope's feed.
	has, err = st.HasEmptySessions(ctx, store.SessionFilter{Agent: "nonesuch"})
	if err != nil {
		t.Fatalf("probe scoped: %v", err)
	}
	if has {
		t.Error("an out-of-scope empty should report false")
	}
}

// TestHasEmptySessionsExcludesSubagent pins the empty-toggle probe to ListAllSessions' default
// subagent hiding: an empty session that is a hidden subagent must not make the probe report an
// empty in scope, because the toggle it drives would still not reveal that row. Including
// subagents (the subagents=1 feed) brings it back into scope, so the probe finds it then.
func TestHasEmptySessionsExcludesSubagent(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/sub", "github.com", "x", "sub", "sub", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// The only empty session in scope is a subagent.
	sub := seedEmptySess(t, st, u.ID, proj, "he-subagent")
	markSubagent(t, st, ctx, sub)

	// Default scope hides subagents, so the toggle would reveal nothing: the probe reports false.
	has, err := st.HasEmptySessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("probe default: %v", err)
	}
	if has {
		t.Error("an empty subagent must not make the default-scope probe report an empty")
	}

	// The subagents=1 feed shows subagents, so the same empty is now revealable: probe true.
	has, err = st.HasEmptySessions(ctx, store.SessionFilter{IncludeSubagents: true})
	if err != nil {
		t.Fatalf("probe with subagents: %v", err)
	}
	if !has {
		t.Error("with subagents included, the empty subagent should report true")
	}
}

// TestListAllSessionsTitle asserts every row carries its first user message as the
// title, whitespace-squashed, with no user message leaving the title empty.
func TestListAllSessionsTitle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	withTitle := seedSess(t, st, u.ID, proj, "claude", "box", "titled")
	// A leading assistant message and a multi-line, multi-space user message: the
	// title is the FIRST USER message, squashed to single-spaced.
	seedMessage(t, st, withTitle, 0, "assistant", "I'll help with that.")
	seedMessage(t, st, withTitle, 1, "user", "Fix   the\n\n  timezone   pass")

	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *store.SessionRow
	for i := range rows {
		if rows[i].ID == withTitle {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("seeded session not listed")
	}
	if got.Title != "Fix the timezone pass" {
		t.Errorf("title = %q, want the squashed first user message", got.Title)
	}
}

// TestCountAllSessionsAgreement asserts CountAllSessions reports the same matching
// total the list returns (the empty-hidden default excluded), and that the empty
// count reflects the zero-message sessions the default hides within scope.
func TestCountAllSessionsAgreement(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Two sessions with messages, one empty (message_count stays 0). The full ones
	// are seeded empty then given a message so message_count reflects exactly one.
	full1 := seedEmptySess(t, st, u.ID, proj, "f1")
	full2 := seedEmptySess(t, st, u.ID, proj, "f2")
	seedEmptySess(t, st, u.ID, proj, "empty")
	seedMessage(t, st, full1, 0, "user", "one")
	seedMessage(t, st, full2, 0, "user", "two")

	// Default (empty hidden): the list shows the two full sessions, the count agrees,
	// and one empty is reported hidden.
	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	total, empty, err := st.CountAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) != 2 || total != 2 {
		t.Fatalf("default: rows=%d total=%d, want 2 and 2", len(rows), total)
	}
	if empty != 1 {
		t.Errorf("default: empty hidden = %d, want 1", empty)
	}

	// IncludeEmpty: the list shows all three, the count agrees, and empty still
	// reports how many of the shown rows are empty.
	incRows, _, err := st.ListAllSessions(ctx, store.SessionFilter{IncludeEmpty: true})
	if err != nil {
		t.Fatalf("list include-empty: %v", err)
	}
	incTotal, incEmpty, err := st.CountAllSessions(ctx, store.SessionFilter{IncludeEmpty: true})
	if err != nil {
		t.Fatalf("count include-empty: %v", err)
	}
	if len(incRows) != 3 || incTotal != 3 {
		t.Fatalf("include-empty: rows=%d total=%d, want 3 and 3", len(incRows), incTotal)
	}
	if incEmpty != 1 {
		t.Errorf("include-empty: empty count = %d, want 1", incEmpty)
	}
}

// TestListAllSessionsHidesEmpty asserts the default excludes zero-message sessions
// and IncludeEmpty restores them.
func TestListAllSessionsHidesEmpty(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	full := seedEmptySess(t, st, u.ID, proj, "full")
	seedEmptySess(t, st, u.ID, proj, "empty")
	seedMessage(t, st, full, 0, "user", "hi")

	hidden, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(hidden) != 1 || hidden[0].ID != full {
		t.Fatalf("default should hide the empty session, got %d rows: %+v", len(hidden), hidden)
	}

	shown, _, err := st.ListAllSessions(ctx, store.SessionFilter{IncludeEmpty: true})
	if err != nil {
		t.Fatalf("include-empty list: %v", err)
	}
	if len(shown) != 2 {
		t.Fatalf("include-empty should show both, got %d rows", len(shown))
	}
}

// TestListAllSessionsRequireSpan pins the span constraint the busiest-user drill
// carries: RequireSpan narrows to sessions with a parsed start and end and a
// non-negative duration, the exact cohort the concurrency panel sweeps, so a session
// missing an ended_at (never counted by the panel) is excluded from the drill feed.
func TestListAllSessionsRequireSpan(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	now := time.Now()
	// A spanned session (start and end set): counted by the panel, kept by the drill.
	spanned := seedSess(t, st, u.ID, proj, "claude", "box", "spanned")
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET started_at = $2, ended_at = $3 WHERE id = $1",
		spanned, now.Add(-time.Hour), now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// An unspanned session (no ended_at): never counted by the panel, dropped by the drill.
	unspanned := seedSess(t, st, u.ID, proj, "claude", "box", "unspanned")
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET started_at = $2, ended_at = NULL WHERE id = $1",
		unspanned, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Without the constraint both list; with it only the spanned one.
	all, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if !idSet(all)[unspanned] {
		t.Errorf("unconstrained list should include the unspanned session %d", unspanned)
	}
	span, _, err := st.ListAllSessions(ctx, store.SessionFilter{RequireSpan: true})
	if err != nil {
		t.Fatalf("list spanned: %v", err)
	}
	got := idSet(span)
	if !got[spanned] || got[unspanned] {
		t.Errorf("RequireSpan = %v, want only the spanned session %d (not %d)", got, spanned, unspanned)
	}
}

// TestGlobalFacetsReconcileEmptyHidden pins the intentional gap between the facet
// rollup and the default feed: the rollup counts every session (empties included),
// while the default feed hides empties, so a facet's count can exceed a default-feed
// click on it by exactly the empty sessions carrying that value. Showing empties
// (IncludeEmpty) reconciles the two: the facet count equals the IncludeEmpty feed count
// and is at least the default feed count. If the rollup ever silently started tracking
// message_count (or the feed stopped hiding empties), this fails.
func TestGlobalFacetsReconcileEmptyHidden(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Two sessions under agent 'claude': one full (given a message), one empty. Both
	// bump the 'claude' facet count, but only the full one shows in the default feed.
	full := seedSess(t, st, u.ID, proj, "claude", "box", "gf-full")
	seedMessage(t, st, full, 0, "user", "hello")
	seedEmptySess(t, st, u.ID, proj, "gf-empty") // seedEmptySess uses agent 'claude'

	facets, err := st.GlobalFacets(ctx)
	if err != nil {
		t.Fatalf("global facets: %v", err)
	}
	var facetCount int
	for _, a := range facets.Agents {
		if a.Value == "claude" {
			facetCount = a.Count
		}
	}
	if facetCount != 2 {
		t.Fatalf("facet count for claude = %d, want 2 (rollup counts empties too)", facetCount)
	}

	// Default feed hides the empty: fewer rows than the facet count.
	def, _, err := st.ListAllSessions(ctx, store.SessionFilter{Agent: "claude"})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(def) != 1 {
		t.Errorf("default feed for claude = %d rows, want 1 (empty hidden)", len(def))
	}
	if len(def) > facetCount {
		t.Errorf("default feed (%d) must not exceed the facet count (%d)", len(def), facetCount)
	}

	// Showing empties reconciles: the IncludeEmpty feed count equals the facet count.
	inc, _, err := st.ListAllSessions(ctx, store.SessionFilter{Agent: "claude", IncludeEmpty: true})
	if err != nil {
		t.Fatalf("include-empty list: %v", err)
	}
	if len(inc) != facetCount {
		t.Errorf("IncludeEmpty feed for claude = %d rows, want the facet count %d", len(inc), facetCount)
	}
}

// TestListAllSessionsCostSort asserts the "cost" sort orders by total_cost_usd in
// both directions, the new sortable column.
func TestListAllSessionsCostSort(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	seedCost := func(src string, cost float64) {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, message_count, total_cost_usd)
			 VALUES ($1,$2,'claude',$3,'box',1,$4) RETURNING id`,
			u.ID, proj, src, cost).Scan(&id); err != nil {
			t.Fatalf("seed cost session: %v", err)
		}
	}
	seedCost("c1", 0.50)
	seedCost("c2", 5.00)
	seedCost("c3", 1.25)

	if !store.IsSortKey("cost") {
		t.Fatal("cost should be a recognized sort key")
	}

	desc, _, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: "cost", Desc: true})
	if err != nil {
		t.Fatalf("cost sort desc: %v", err)
	}
	for i := 1; i < len(desc); i++ {
		if desc[i-1].TotalCostUSD < desc[i].TotalCostUSD {
			t.Fatalf("cost desc out of order at %d: %.2f then %.2f", i, desc[i-1].TotalCostUSD, desc[i].TotalCostUSD)
		}
	}
	if len(desc) == 0 || desc[0].TotalCostUSD != 5.00 {
		t.Errorf("most expensive should lead, got %.2f", desc[0].TotalCostUSD)
	}

	asc, _, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: "cost", Desc: false})
	if err != nil {
		t.Fatalf("cost sort asc: %v", err)
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1].TotalCostUSD > asc[i].TotalCostUSD {
			t.Fatalf("cost asc out of order at %d: %.2f then %.2f", i, asc[i-1].TotalCostUSD, asc[i].TotalCostUSD)
		}
	}
}
