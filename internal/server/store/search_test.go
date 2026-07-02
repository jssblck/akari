package store_test

import (
	"context"
	"strings"
	"testing"

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
	rows, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "PRICING"})
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
	pct, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "95%"})
	if err != nil {
		t.Fatalf("search '95%%': %v", err)
	}
	if len(pct) != 1 || pct[0].ID != literal {
		t.Fatalf("search '95%%' = %d rows (want 1, session %d)", len(pct), literal)
	}
	// A bare '%' is a literal percent, not the match-anything wildcard: it matches
	// only the one session whose content contains a literal '%' (the "95%" row), not
	// every session. This is the escaping working: an unescaped '%' would match all.
	bare, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "%"})
	if err != nil {
		t.Fatalf("search bare '%%': %v", err)
	}
	if len(bare) != 1 || bare[0].ID != literal {
		t.Errorf("a bare '%%' should match only the literal-percent session, got %d rows", len(bare))
	}

	// The '_' is a literal too: "cache_read" matches only the literal session, and
	// it must not behave like the single-character wildcard "cache?read".
	under, err := st.ListAllSessions(ctx, store.SessionFilter{Query: "cache_read"})
	if err != nil {
		t.Fatalf("search 'cache_read': %v", err)
	}
	if len(under) != 1 || under[0].ID != literal {
		t.Fatalf("search 'cache_read' = %d rows (want 1, session %d)", len(under), literal)
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

	rows, err := st.ListAllSessions(ctx, store.SessionFilter{})
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
	rows, err := st.ListAllSessions(ctx, store.SessionFilter{})
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
	incRows, err := st.ListAllSessions(ctx, store.SessionFilter{IncludeEmpty: true})
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

	hidden, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(hidden) != 1 || hidden[0].ID != full {
		t.Fatalf("default should hide the empty session, got %d rows: %+v", len(hidden), hidden)
	}

	shown, err := st.ListAllSessions(ctx, store.SessionFilter{IncludeEmpty: true})
	if err != nil {
		t.Fatalf("include-empty list: %v", err)
	}
	if len(shown) != 2 {
		t.Fatalf("include-empty should show both, got %d rows", len(shown))
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

	desc, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: "cost", Desc: true})
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

	asc, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: "cost", Desc: false})
	if err != nil {
		t.Fatalf("cost sort asc: %v", err)
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1].TotalCostUSD > asc[i].TotalCostUSD {
			t.Fatalf("cost asc out of order at %d: %.2f then %.2f", i, asc[i-1].TotalCostUSD, asc[i].TotalCostUSD)
		}
	}
}
