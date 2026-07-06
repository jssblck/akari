package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// setActive stamps a session's last-activity instant, the column the default feed order and
// its keyset cursor both walk. last_active_at is generated as COALESCE(ended_at, created_at),
// so the test drives it through ended_at; Announce lands every seeded session at nearly the
// same instant, so a keyset test has to spread them itself to exercise the column comparison,
// not just the id tiebreak.
func setActive(t *testing.T, st *store.Store, ctx context.Context, sid int64, at time.Time) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET ended_at = $2 WHERE id = $1`, sid, at); err != nil {
		t.Fatalf("set ended_at for session %d: %v", sid, err)
	}
}

// TestListAllSessionsKeyset drives the feed's keyset pagination end to end: walking the pages
// with an After cursor visits every row exactly once, in the same order a single unpaginated
// read returns, with no overlap or gap across page boundaries. It exercises the column
// comparison (distinct activity instants), the id tiebreak (a deliberate tie), and a
// non-default sort column (tokens), the three ways the predicate can go wrong.
func TestListAllSessionsKeyset(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// Six sessions with distinct activity instants, except s3 and s6 share one so the
	// tiebreak has to order them by id. Newest-first, the order is s5, s4, then the tie
	// pair (higher id first under the descending id tiebreak), then s2, s1.
	s1 := seedSession(t, st, uid, pid, "k1")
	setActive(t, st, ctx, s1, base.Add(1*time.Minute))
	s2 := seedSession(t, st, uid, pid, "k2")
	setActive(t, st, ctx, s2, base.Add(2*time.Minute))
	s3 := seedSession(t, st, uid, pid, "k3")
	setActive(t, st, ctx, s3, base.Add(3*time.Minute))
	s4 := seedSession(t, st, uid, pid, "k4")
	setActive(t, st, ctx, s4, base.Add(4*time.Minute))
	s5 := seedSession(t, st, uid, pid, "k5")
	setActive(t, st, ctx, s5, base.Add(5*time.Minute))
	s6 := seedSession(t, st, uid, pid, "k6")
	setActive(t, st, ctx, s6, base.Add(3*time.Minute)) // ties s3 on the activity column

	// Give the sessions distinct costs whose order does not follow their ids, so the cost
	// sort (a numeric keyset column) walks a genuinely different order than the id tiebreak
	// would and a predicate that leaned on id alone would be caught.
	for sid, c := range map[int64]float64{s1: 5, s2: 1, s3: 6, s4: 2, s5: 4, s6: 3} {
		setSessionCost(t, st, ctx, sid, c)
	}

	// walk pages the way "Show more" does: fetch pageSize rows, then resume after the last
	// row's id, until a page reports no more. It returns the ids visited in order.
	walk := func(sort string, pageSize int) []int64 {
		var got []int64
		var after int64
		for {
			f := store.SessionFilter{ProjectID: pid, IncludeEmpty: true, Sort: sort, Desc: true, Limit: pageSize, After: after}
			rows, hasMore, err := st.ListAllSessions(ctx, f)
			if err != nil {
				t.Fatalf("list (%s, after %d): %v", sort, after, err)
			}
			if len(rows) == 0 {
				break
			}
			for _, r := range rows {
				got = append(got, r.ID)
			}
			if !hasMore {
				break
			}
			after = rows[len(rows)-1].ID
		}
		return got
	}

	// The unpaginated order is the ground truth the paged walk must reproduce exactly.
	full, _, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true, Sort: "updated", Desc: true, Limit: 100})
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	var want []int64
	for _, r := range full {
		want = append(want, r.ID)
	}
	if len(want) != 6 {
		t.Fatalf("expected 6 sessions in scope, got %d", len(want))
	}
	// The tie pair (s3, s6) must sit adjacent, ordered by descending id, so the tiebreak is
	// actually under test rather than incidentally satisfied.
	assertAdjacentTie(t, want, s3, s6)

	// A page size of 2 forces the cursor across three boundaries, including one that lands
	// mid-tie (between s6 and s3 if they straddle a page), the case a column-only predicate
	// would drop or duplicate.
	for _, pageSize := range []int{1, 2, 4} {
		got := walk("updated", pageSize)
		assertSameOrder(t, got, want, "updated", pageSize)
	}

	// Cost is a different keyset column (a numeric one), and its order deliberately does not
	// follow the ids, so pagination must resume by the cost value, not the id. Its ground
	// truth is its own unpaginated read.
	fullCost, _, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true, Sort: "cost", Desc: true, Limit: 100})
	if err != nil {
		t.Fatalf("full cost list: %v", err)
	}
	var wantCost []int64
	for _, r := range fullCost {
		wantCost = append(wantCost, r.ID)
	}
	// The cost order is s3, s1, s5, s6, s4, s2 by the costs set above, not the id order.
	if wantCost[0] != s3 || wantCost[len(wantCost)-1] != s2 {
		t.Fatalf("cost order should lead with s3 and end with s2, got %v", wantCost)
	}
	assertSameOrder(t, walk("cost", 2), wantCost, "cost", 2)

	// A stale cursor (an id that names no session) ends the walk cleanly rather than
	// resuming from a wrong place or erroring.
	rows, hasMore, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true, Sort: "updated", Desc: true, Limit: 2, After: 9_999_999})
	if err != nil {
		t.Fatalf("stale cursor list: %v", err)
	}
	if len(rows) != 0 || hasMore {
		t.Errorf("a stale cursor should yield an empty final page, got %d rows hasMore=%v", len(rows), hasMore)
	}
}

// TestListAllSessionsKeysetStableUnderActivity pins the fix for a drifting keyset cursor. The
// "Show more" boundary is the sort value the page rendered (SessionFilter.AfterVal), so new
// activity on the cursor row between pages (which bumps its last_active_at) cannot move the
// boundary and re-show or skip rows. The bare id cursor resolves the boundary live from the
// cursor row, so it drifts when that row moves; this shows the carried value holds where the
// live lookup does not.
func TestListAllSessionsKeysetStableUnderActivity(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	s1 := seedSession(t, st, uid, pid, "a1")
	setActive(t, st, ctx, s1, base.Add(1*time.Minute))
	s2 := seedSession(t, st, uid, pid, "a2")
	setActive(t, st, ctx, s2, base.Add(2*time.Minute))
	s3 := seedSession(t, st, uid, pid, "a3")
	setActive(t, st, ctx, s3, base.Add(3*time.Minute))
	s4 := seedSession(t, st, uid, pid, "a4")
	setActive(t, st, ctx, s4, base.Add(4*time.Minute))
	s5 := seedSession(t, st, uid, pid, "a5")
	setActive(t, st, ctx, s5, base.Add(5*time.Minute))

	// Page 1 of the recency feed: newest first, s5, s4, s3. Its last row (s3) is the cursor.
	page1, hasMore, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true, Sort: "updated", Desc: true, Limit: 3})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	assertSameOrder(t, rowIDs(page1), []int64{s5, s4, s3}, "updated", 3)
	if !hasMore {
		t.Fatal("page 1 should report more rows")
	}
	cursor := page1[len(page1)-1] // s3
	if cursor.LastActiveAt == nil {
		t.Fatal("cursor row has no last_active_at")
	}
	// The value the web footer carries: the cursor row's activity instant as page 1 saw it,
	// formatted exactly as web.keysetCursorValue does for the default order.
	cursorVal := cursor.LastActiveAt.UTC().Format(time.RFC3339Nano)

	// Between page loads, the cursor row gets new activity and jumps to the top of the order.
	setActive(t, st, ctx, s3, base.Add(10*time.Minute))

	// Page 2 carrying the observed value resumes at the boundary the reader actually saw, so it
	// returns exactly the rows below it (s2, s1) and overlaps page 1 nowhere.
	stable, _, err := st.ListAllSessions(ctx, store.SessionFilter{
		ProjectID: pid, IncludeEmpty: true, Sort: "updated", Desc: true, Limit: 3,
		After: cursor.ID, AfterVal: cursorVal,
	})
	if err != nil {
		t.Fatalf("stable page 2: %v", err)
	}
	assertSameOrder(t, rowIDs(stable), []int64{s2, s1}, "updated-stable", 3)

	// The bare id cursor resolves the boundary live from the now-bumped cursor row, so it drifts
	// and re-includes rows page 1 already showed. This is the behavior AfterVal exists to prevent.
	drifted, _, err := st.ListAllSessions(ctx, store.SessionFilter{
		ProjectID: pid, IncludeEmpty: true, Sort: "updated", Desc: true, Limit: 3,
		After: cursor.ID,
	})
	if err != nil {
		t.Fatalf("drifted page 2: %v", err)
	}
	var reshown bool
	for _, id := range rowIDs(drifted) {
		if id == s4 || id == s5 {
			reshown = true
		}
	}
	if !reshown {
		t.Errorf("expected the id-only cursor to drift and re-show a page-1 row, got %v", rowIDs(drifted))
	}
}

func rowIDs(rows []store.SessionRow) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func assertSameOrder(t *testing.T, got, want []int64, sort string, pageSize int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("keyset walk (%s, page %d) visited %d rows, want %d: got %v want %v", sort, pageSize, len(got), len(want), got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keyset walk (%s, page %d) row %d = %d, want %d (full got %v want %v)", sort, pageSize, i, got[i], want[i], got, want)
			return
		}
	}
}

func assertAdjacentTie(t *testing.T, order []int64, a, b int64) {
	t.Helper()
	hi, lo := a, b
	if lo > hi {
		hi, lo = lo, hi
	}
	for i := 0; i+1 < len(order); i++ {
		if order[i] == hi && order[i+1] == lo {
			return
		}
	}
	t.Fatalf("expected the tie pair (%d before %d, higher id first) adjacent in %v", hi, lo, order)
}
