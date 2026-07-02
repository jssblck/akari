package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedSess inserts a session with a chosen agent and machine under a user and
// project, bypassing ingest so the cross-project read paths can be asserted
// against known inputs. Rows are returned newest-id last.
//
// It seeds message_count = 1 so the session is non-empty: the global feed hides
// zero-message sessions by default, so a plain seeded session must read as a real
// one. Tests that want an EMPTY session (to exercise the hide/toggle) seed one
// directly with message_count left at its 0 default rather than through here.
func seedSess(t *testing.T, st *store.Store, userID, projectID int64, agent, machine, src string) int64 {
	t.Helper()
	var id int64
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, message_count)
		 VALUES ($1,$2,$3,$4,$5,1) RETURNING id`,
		userID, projectID, agent, src, machine).Scan(&id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

// seedEmptySess inserts a zero-message session (message_count stays 0), the kind
// the global feed hides by default. It is the counterpart to seedSess for the
// empty-hide and toggle assertions.
func seedEmptySess(t *testing.T, st *store.Store, userID, projectID int64, src string) int64 {
	t.Helper()
	var id int64
	if err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1,$2,'claude',$3,'box') RETURNING id`,
		userID, projectID, src).Scan(&id); err != nil {
		t.Fatalf("seed empty session: %v", err)
	}
	return id
}

// seedSortSess inserts a session with full control over every sortable column, so
// the click-to-sort ordering can be asserted against known values rather than the
// zeros seedSess leaves. ageMin places updated_at that many minutes in the past, so
// a larger ageMin reads as a less recently active session. Returns the new id;
// sessions seeded later carry larger ids, which the direction-following tiebreak
// orders within ties.
func seedSortSess(t *testing.T, st *store.Store, userID, projectID int64, agent, branch, src string, msgs int, in, out, cr, cw int64, ageMin int) int64 {
	t.Helper()
	var id int64
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		   git_branch, message_count,
		   total_input_tokens, total_output_tokens, total_cache_read_tokens, total_cache_write_tokens,
		   updated_at)
		 VALUES ($1,$2,$3,$4,'box',$5,$6,$7,$8,$9,$10, now() - make_interval(mins => $11))
		 RETURNING id`,
		userID, projectID, agent, src, branch, msgs, in, out, cr, cw, ageMin).Scan(&id)
	if err != nil {
		t.Fatalf("seed sort session: %v", err)
	}
	return id
}

// seedGlobalCorpus sets up one user, a remote and a local project, and six
// sessions: four claude / two codex, with one claude session carrying a blank
// machine to exercise the facet's blank exclusion. Returns the two project ids.
func seedGlobalCorpus(t *testing.T, st *store.Store) (userID, remoteID, localID int64) {
	t.Helper()
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	remoteID, err = st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("remote project: %v", err)
	}
	localID, err = st.UpsertProject(ctx, "local:rig:/home/grace/scratch", "rig", "", "scratch", "scratch", "standalone")
	if err != nil {
		t.Fatalf("local project: %v", err)
	}
	seedSess(t, st, u.ID, remoteID, "claude", "box", "a1")
	seedSess(t, st, u.ID, remoteID, "claude", "box", "a2")
	seedSess(t, st, u.ID, remoteID, "claude", "box", "a3")
	seedSess(t, st, u.ID, localID, "codex", "rig", "b1")
	seedSess(t, st, u.ID, localID, "codex", "rig", "b2")
	seedSess(t, st, u.ID, localID, "claude", "", "b3") // blank machine: excluded from machine facet
	return u.ID, remoteID, localID
}

// TestListProjectsRollups asserts the projects-index rollup: session counts and
// all four token classes (input, output, cache read, cache write) sum per
// project, and the synthetic TotalTokens reduces them to the single figure the
// index shows. Two sessions are seeded so the sums are not trivially one row.
func TestListProjectsRollups(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projID, err := st.UpsertProject(ctx, "github.com/ada-lovelace/engine", "github.com", "ada-lovelace", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	seedTokens := func(src string, in, out, cr, cw int64, cost float64) {
		t.Helper()
		_, err := st.Pool.Exec(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
			   total_input_tokens, total_output_tokens, total_cache_read_tokens,
			   total_cache_write_tokens, total_cost_usd)
			 VALUES ($1,$2,'claude',$3,'box',$4,$5,$6,$7,$8)`,
			u.ID, projID, src, in, out, cr, cw, cost)
		if err != nil {
			t.Fatalf("seed tokens: %v", err)
		}
	}
	seedTokens("s1", 100, 50, 30, 20, 1.25)
	seedTokens("s2", 400, 150, 70, 80, 3.75)

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	var got *store.ProjectSummary
	for i := range projects {
		if projects[i].ID == projID {
			got = &projects[i]
		}
	}
	if got == nil {
		t.Fatal("seeded project not in ListProjects result")
	}
	if got.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", got.SessionCount)
	}
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"TotalInput", got.TotalInput, 500},
		{"TotalOutput", got.TotalOutput, 200},
		{"TotalCacheRead", got.TotalCacheRead, 100},
		{"TotalCacheWrite", got.TotalCacheWrite, 100},
		{"TotalTokens", got.TotalTokens(), 900},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestListAllSessions(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	_, remoteID, _ := seedGlobalCorpus(t, st)

	all, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("len(all) = %d, want 6", len(all))
	}
	// Newest id first (no explicit updated_at, so ordering falls to the id
	// tiebreak), and the project identity travels with each row.
	if all[0].ID <= all[len(all)-1].ID {
		t.Errorf("rows not newest-first: %d then %d", all[0].ID, all[len(all)-1].ID)
	}
	for _, r := range all {
		if r.ProjectID == 0 || r.ProjectKey == "" || r.ProjectKind == "" {
			t.Errorf("row %d missing project fields: %+v", r.ID, r)
		}
	}

	// Filters narrow the set.
	claude, _, err := st.ListAllSessions(ctx, store.SessionFilter{Agent: "claude"})
	if err != nil || len(claude) != 4 {
		t.Fatalf("agent filter: len=%d err=%v, want 4", len(claude), err)
	}
	inRemote, _, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: remoteID})
	if err != nil || len(inRemote) != 3 {
		t.Fatalf("project filter: len=%d err=%v, want 3", len(inRemote), err)
	}
	byMachine, _, err := st.ListAllSessions(ctx, store.SessionFilter{Machine: "rig"})
	if err != nil || len(byMachine) != 2 {
		t.Fatalf("machine filter: len=%d err=%v, want 2", len(byMachine), err)
	}

	// Limit caps the page.
	capped, _, err := st.ListAllSessions(ctx, store.SessionFilter{Limit: 2})
	if err != nil || len(capped) != 2 {
		t.Fatalf("limit: len=%d err=%v, want 2", len(capped), err)
	}
}

// TestListAllSessionsSort exercises the click-to-sort ordering across every
// sortable column, in both directions, including the keys that sort on joined or
// computed values: project (a CASE over the projects table) and tokens (the sum of
// the four token classes, now read from the generated total_tokens column). Each
// column must order its rows by that column's value, the direction flag must flip
// it, and ties must break by id in the same direction the column sorts (the
// property that lets one (col, id) index serve both directions). An unknown key
// falls back to the default, most-recent-first order.
func TestListAllSessionsSort(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Two users to exercise the user sort. They go in directly (the invite-gated
	// Register flow is covered elsewhere) so the test controls both usernames.
	seedUser := func(name string) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO users (username, password_hash, is_admin) VALUES ($1, 'x', FALSE) RETURNING id`,
			name).Scan(&id); err != nil {
			t.Fatalf("seed user %q: %v", name, err)
		}
		return id
	}
	adaID := seedUser("ada")
	graceID := seedUser("grace")
	remoteID, err := st.UpsertProject(ctx, "github.com/ada-lovelace/engine", "github.com", "ada-lovelace", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("remote project: %v", err)
	}
	localID, err := st.UpsertProject(ctx, "local:rig:/home/grace/scratch", "rig", "", "scratch", "scratch", "standalone")
	if err != nil {
		t.Fatalf("local project: %v", err)
	}

	// Five sessions whose sortable columns are deliberately varied: branch, tokens,
	// and updated are all-distinct, while agent, user, project, and messages carry
	// ties so the direction-following id tiebreak is exercised too.
	//                  user      project   agent     branch   src   msgs   in   out   cr   cw  ageMin
	seedSortSess(t, st, adaID, remoteID, "claude", "main", "s1", 10, 10, 10, 10, 10, 5)
	seedSortSess(t, st, adaID, remoteID, "claude", "dev", "s2", 30, 100, 0, 0, 0, 1)
	seedSortSess(t, st, graceID, localID, "codex", "alpha", "s3", 5, 0, 0, 0, 5, 9)
	seedSortSess(t, st, graceID, localID, "pi", "zeta", "s4", 20, 50, 50, 50, 50, 3)
	seedSortSess(t, st, adaID, remoteID, "codex", "beta", "s5", 5, 7, 8, 9, 10, 7)
	// token sums: s1=40, s2=100, s3=5, s4=200, s5=34 (all distinct)

	// projectSortKey mirrors the project sort's CASE: local kinds sort by display
	// name, remotes by remote key.
	projectSortKey := func(r store.SessionRow) string {
		if r.ProjectKind == "standalone" || r.ProjectKind == "orphaned" {
			return r.ProjectName
		}
		return r.ProjectKey
	}
	tokenSum := func(r store.SessionRow) int64 {
		return r.TotalInput + r.TotalOutput + r.TotalCacheRead + r.TotalCacheWrite
	}
	cmpInt := func(a, b int) int { return cmpOrd(int64(a), int64(b)) }

	// cmp returns the sign of a-b by the column the key sorts on, ignoring the id
	// tiebreak, so a tie (0) lets the assertion check the id ordering separately.
	cases := []struct {
		key string
		cmp func(a, b store.SessionRow) int
	}{
		{"agent", func(a, b store.SessionRow) int { return strings.Compare(a.Agent, b.Agent) }},
		{"branch", func(a, b store.SessionRow) int { return strings.Compare(a.GitBranch, b.GitBranch) }},
		{"user", func(a, b store.SessionRow) int { return strings.Compare(a.Username, b.Username) }},
		{"project", func(a, b store.SessionRow) int { return strings.Compare(projectSortKey(a), projectSortKey(b)) }},
		{"messages", func(a, b store.SessionRow) int { return cmpInt(a.MessageCount, b.MessageCount) }},
		{"tokens", func(a, b store.SessionRow) int { return cmpOrd(tokenSum(a), tokenSum(b)) }},
		{"updated", func(a, b store.SessionRow) int {
			switch {
			case a.UpdatedAt.Before(*b.UpdatedAt):
				return -1
			case a.UpdatedAt.After(*b.UpdatedAt):
				return 1
			default:
				return 0
			}
		}},
	}

	assertOrdered := func(t *testing.T, key string, cmp func(a, b store.SessionRow) int, desc bool) {
		t.Helper()
		rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: key, Desc: desc})
		if err != nil {
			t.Fatalf("sort %s desc=%v: %v", key, desc, err)
		}
		if len(rows) != 5 {
			t.Fatalf("sort %s desc=%v: got %d rows, want 5", key, desc, len(rows))
		}
		for i := 1; i < len(rows); i++ {
			prev, cur := rows[i-1], rows[i]
			c := cmp(prev, cur)
			if desc {
				c = -c
			}
			if c > 0 {
				t.Fatalf("sort %s desc=%v: rows out of column order at %d (ids %d then %d)", key, desc, i, prev.ID, cur.ID)
			}
			if c == 0 { // a tie: the id tiebreak must follow the sort direction
				if !desc && prev.ID > cur.ID {
					t.Fatalf("sort %s asc: tie not broken by ascending id at %d (%d then %d)", key, i, prev.ID, cur.ID)
				}
				if desc && prev.ID < cur.ID {
					t.Fatalf("sort %s desc: tie not broken by descending id at %d (%d then %d)", key, i, prev.ID, cur.ID)
				}
			}
		}
	}

	for _, tc := range cases {
		assertOrdered(t, tc.key, tc.cmp, false)
		assertOrdered(t, tc.key, tc.cmp, true)
	}

	// An unknown sort key falls back to the default order (most recent first, id
	// descending on ties), identical to the zero-value filter.
	bogus, _, err := st.ListAllSessions(ctx, store.SessionFilter{Sort: "; drop table sessions"})
	if err != nil {
		t.Fatalf("bogus sort should fall back, not error: %v", err)
	}
	def, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(bogus) != len(def) {
		t.Fatalf("bogus sort len=%d, default len=%d", len(bogus), len(def))
	}
	for i := range def {
		if bogus[i].ID != def[i].ID {
			t.Fatalf("bogus sort did not fall back to default order at %d: %d vs %d", i, bogus[i].ID, def[i].ID)
		}
	}
}

// TestListAllSessionsOutcomeGradeFilter confirms the SessionFilter outcome and grade
// narrowing mirrors the Insights distribution buckets: a graded, completed session matches
// outcome=completed and its letter; an ungraded session (no gated signals row) matches only
// outcome=unknown and grade=unscored, since the filter coalesces a missing row to those
// buckets exactly as the distribution LEFT JOIN does. This is the property that keeps a
// distribution bar's count and its drill-down list in agreement.
func TestListAllSessionsOutcomeGradeFilter(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// A graded, completed session (grade B) and an ungraded one (no signals row at all, so it
	// coalesces to unknown/unscored). markSignalsFresh inside insertSignal clears signals_stale
	// so the graded row reads through the version-and-stale gate the filter applies.
	graded := seedSession(t, st, ada, pid, "graded")
	insertSignal(t, st, ctx, graded, quality.Version, "completed", "B")
	ungraded := seedSession(t, st, ada, pid, "ungraded")

	// The seeded sessions carry no message, so each filter sets IncludeEmpty to see them, the
	// same empty=1 the real quality drill-downs carry so a bar's count and its feed agree
	// regardless of message_count (a zero-message session can still carry a grade).
	only := func(t *testing.T, f store.SessionFilter, want int64, label string) {
		t.Helper()
		f.IncludeEmpty = true
		rows, _, err := st.ListAllSessions(ctx, f)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(rows) != 1 || rows[0].ID != want {
			ids := make([]int64, len(rows))
			for i, r := range rows {
				ids[i] = r.ID
			}
			t.Fatalf("%s = %v, want just [%d]", label, ids, want)
		}
	}

	only(t, store.SessionFilter{Outcome: "completed"}, graded, "outcome=completed")
	only(t, store.SessionFilter{Outcome: "unknown"}, ungraded, "outcome=unknown")
	only(t, store.SessionFilter{Grade: "B"}, graded, "grade=B")
	only(t, store.SessionFilter{Grade: "unscored"}, ungraded, "grade=unscored")

	// A stale graded row folds into the missing bucket: mark the graded session stale and it
	// now matches unknown/unscored rather than its stored outcome and grade, the read-side
	// mirror of the settle pass's staleness flag.
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, graded); err != nil {
		t.Fatalf("mark graded stale: %v", err)
	}
	if rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Outcome: "completed", IncludeEmpty: true}); err != nil {
		t.Fatalf("stale outcome=completed: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("stale graded session should not match outcome=completed, got %d rows", len(rows))
	}
	if rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Grade: "unscored", IncludeEmpty: true}); err != nil {
		t.Fatalf("stale grade=unscored: %v", err)
	} else if len(rows) != 2 {
		t.Errorf("both sessions should read unscored once the graded one is stale, got %d rows", len(rows))
	}
}

// cmpOrd returns the sign of a-b, for asserting numeric column orderings.
func cmpOrd(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// TestListSessionsSince bounds a project's session list to a trailing window by
// last activity: a session active inside the window shows, one older does not, and a
// zero Since lists both. (The project page itself now windows by usage date through
// WindowSessions, so its table partitions the usage panel; ListSessions remains the
// recency-windowed project query and the global feed shares its Since handling.)
func TestListSessionsSince(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	recent := seedSess(t, st, u.ID, projectID, "claude", "box", "recent")
	old := seedSess(t, st, u.ID, projectID, "claude", "box", "old")
	// Age both timestamps together: the per-project ListSessions windows by updated_at
	// while the global ListAllSessions windows by started_at (matching the Insights
	// panels), so the test moves both to keep the recent-vs-old split valid for both
	// queries it exercises below.
	age := func(id int64, days int) {
		if _, err := st.Pool.Exec(ctx,
			`UPDATE sessions SET updated_at = now() - make_interval(days => $2),
			        started_at = now() - make_interval(days => $2) WHERE id = $1`,
			id, days); err != nil {
			t.Fatalf("age session %d: %v", id, err)
		}
	}
	age(recent, 1)
	age(old, 40)

	// No bound lists both sessions.
	all, err := st.ListSessions(ctx, store.SessionFilter{ProjectID: projectID})
	if err != nil || len(all) != 2 {
		t.Fatalf("unbounded: len=%d err=%v, want 2", len(all), err)
	}

	// A 30-day window keeps only the recently active session.
	win, err := st.ListSessions(ctx, store.SessionFilter{
		ProjectID: projectID, Since: time.Now().AddDate(0, 0, -30),
	})
	if err != nil {
		t.Fatalf("windowed: %v", err)
	}
	if len(win) != 1 || win[0].ID != recent {
		t.Fatalf("windowed list = %+v, want only the recent session %d", win, recent)
	}

	// The cross-project query honors Since the same way: unbounded lists both, a
	// 30-day window drops the 40-day-old session.
	allRows, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
	if err != nil || len(allRows) != 2 {
		t.Fatalf("global unbounded: len=%d err=%v, want 2", len(allRows), err)
	}
	winRows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Since: time.Now().AddDate(0, 0, -30)})
	if err != nil {
		t.Fatalf("global windowed: %v", err)
	}
	if len(winRows) != 1 || winRows[0].ID != recent {
		t.Fatalf("global windowed list = %+v, want only the recent session %d", winRows, recent)
	}
}

// TestSessionFacetTrigger exercises the rollup trigger directly: an insert counts
// up, a re-attribution (project change) shifts the count, and a delete counts
// down and drops the emptied value.
func TestSessionFacetTrigger(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	a, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project a: %v", err)
	}
	b, err := st.UpsertProject(ctx, "github.com/x/b", "github.com", "x", "b", "b", "remote")
	if err != nil {
		t.Fatalf("project b: %v", err)
	}
	count := func(kind, key string) int {
		var n int
		err := st.Pool.QueryRow(ctx, "SELECT coalesce((SELECT n FROM session_facets WHERE kind=$1 AND key=$2),0)", kind, key).Scan(&n)
		if err != nil {
			t.Fatalf("count %s/%s: %v", kind, key, err)
		}
		return n
	}

	id := seedSess(t, st, u.ID, a, "claude", "box", "s1")
	if count("project", strconv.FormatInt(a, 10)) != 1 || count("agent", "claude") != 1 {
		t.Fatalf("after insert: project a=%d agent claude=%d", count("project", strconv.FormatInt(a, 10)), count("agent", "claude"))
	}

	// Re-attribute the session from project a to project b.
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET project_id=$1 WHERE id=$2", b, id); err != nil {
		t.Fatalf("reattribute: %v", err)
	}
	if count("project", strconv.FormatInt(a, 10)) != 0 || count("project", strconv.FormatInt(b, 10)) != 1 {
		t.Fatalf("after reattribute: a=%d b=%d", count("project", strconv.FormatInt(a, 10)), count("project", strconv.FormatInt(b, 10)))
	}
	if count("agent", "claude") != 1 {
		t.Errorf("agent count should be unchanged by a project move: %d", count("agent", "claude"))
	}

	// Deleting the session drops every facet it backed.
	if _, err := st.Pool.Exec(ctx, "DELETE FROM sessions WHERE id=$1", id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if count("project", strconv.FormatInt(b, 10)) != 0 || count("agent", "claude") != 0 {
		t.Errorf("after delete: project b=%d agent claude=%d", count("project", strconv.FormatInt(b, 10)), count("agent", "claude"))
	}
}

func TestGlobalFacets(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedGlobalCorpus(t, st)

	f, err := st.GlobalFacets(ctx)
	if err != nil {
		t.Fatalf("facets: %v", err)
	}

	// Agents: claude (4) ahead of codex (2), busiest first.
	if len(f.Agents) != 2 || f.Agents[0].Value != "claude" || f.Agents[0].Count != 4 {
		t.Fatalf("agent facet = %+v, want claude=4 first", f.Agents)
	}
	if f.Agents[1].Value != "codex" || f.Agents[1].Count != 2 {
		t.Errorf("agent facet codex = %+v, want 2", f.Agents[1])
	}

	// Machines: blank machine is excluded, so only box (3) and rig (2).
	if len(f.Machines) != 2 {
		t.Fatalf("machine facet = %+v, want 2 (blank excluded)", f.Machines)
	}
	for _, m := range f.Machines {
		if m.Value == "" {
			t.Errorf("machine facet includes blank: %+v", f.Machines)
		}
	}

	// Users: the single owner with all six sessions.
	if len(f.Users) != 1 || f.Users[0].Value != "grace" || f.Users[0].Count != 6 {
		t.Errorf("user facet = %+v, want grace=6", f.Users)
	}

	// Projects: both projects with three sessions each, the git-remote project
	// ordered ahead of the standalone folder even though their counts tie.
	if len(f.Projects) != 2 {
		t.Fatalf("project facet = %+v, want 2", f.Projects)
	}
	for _, p := range f.Projects {
		if p.Count != 3 {
			t.Errorf("project %q count = %d, want 3", p.Key, p.Count)
		}
	}
	if f.Projects[0].Kind != "remote" {
		t.Errorf("project facet order = [%q (%s), %q (%s)], want the remote project first",
			f.Projects[0].Key, f.Projects[0].Kind, f.Projects[1].Key, f.Projects[1].Kind)
	}
}

// TestGlobalFacetsProjectOrder exercises the full ordering contract with all
// three kinds present and counts that would interleave the groups if sorted by
// count alone: every git-remote project ranks above every standalone or orphaned
// folder, and within each group the busier project still comes first.
func TestGlobalFacetsProjectOrder(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// A local folder is the busiest of all, so a count-only sort would float it
	// to the top; the kind grouping must override that.
	remoteA, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("remote a: %v", err)
	}
	remoteB, err := st.UpsertProject(ctx, "github.com/x/b", "github.com", "x", "b", "b", "remote")
	if err != nil {
		t.Fatalf("remote b: %v", err)
	}
	standalone, err := st.UpsertProject(ctx, "local:rig:/home/grace/scratch", "rig", "", "scratch", "scratch", "standalone")
	if err != nil {
		t.Fatalf("standalone: %v", err)
	}
	orphaned, err := st.UpsertProject(ctx, "local:rig:/home/grace/gone", "rig", "", "gone", "gone", "orphaned")
	if err != nil {
		t.Fatalf("orphaned: %v", err)
	}

	// Counts: remoteB=2, remoteA=1, standalone=4, orphaned=3. Busiest-first inside
	// each group gives [remoteB, remoteA] then [standalone, orphaned].
	seed := func(projectID int64, n int) {
		for i := 0; i < n; i++ {
			seedSess(t, st, u.ID, projectID, "claude", "box", fmt.Sprintf("p%d-%d", projectID, i))
		}
	}
	seed(remoteA, 1)
	seed(remoteB, 2)
	seed(standalone, 4)
	seed(orphaned, 3)

	f, err := st.GlobalFacets(ctx)
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	wantIDs := []int64{remoteB, remoteA, standalone, orphaned}
	if len(f.Projects) != len(wantIDs) {
		t.Fatalf("project facet = %+v, want %d entries", f.Projects, len(wantIDs))
	}
	for i, want := range wantIDs {
		if f.Projects[i].ID != want {
			gotOrder := make([]string, len(f.Projects))
			for j, p := range f.Projects {
				gotOrder[j] = fmt.Sprintf("%s(%s,%d)", p.Key, p.Kind, p.Count)
			}
			t.Fatalf("project facet order = %v, want remotes busiest-first then locals busiest-first", gotOrder)
		}
	}
}

// TestMessagesPromptFactsEmptyContent pins that an empty-content user turn reads
// PromptFactsCurrent = false even when its facts are stamped at the current classifier version.
// The Messages read gates PromptFactsCurrent on content_length > 0 so the per-message hygiene
// badge is computed over the same prompt set as gatherPromptHygiene (which counts only user turns
// with content_length > 0). Without the gate, an empty or attachment-only user turn would render a
// transcript badge the session aggregate excluded, so the two would disagree on the same session.
func TestMessagesPromptFactsEmptyContent(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	// An empty-content user row with a digest at the current facts version: everything the version
	// gate checks passes except content_length, which is zero (content is ''). content_length is a
	// generated octet_length(content) column, so inserting '' makes it 0 without setting it directly.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, prompt_short, prompt_no_code, prompt_digest, prompt_facts_version)
		 VALUES ($1, 0, 'user', '', true, false, 123456, $2)`,
		sid, quality.PromptFactsVersion); err != nil {
		t.Fatalf("insert empty-content user message: %v", err)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("read %d messages, want 1", len(msgs))
	}
	// The empty turn has current facts and a digest, but content_length is 0, so the badge gate
	// excludes it exactly as the session hygiene aggregate does.
	if msgs[0].PromptFactsCurrent {
		t.Error("an empty-content user turn must read PromptFactsCurrent = false so the transcript badge matches gatherPromptHygiene's content_length > 0 set")
	}
}
