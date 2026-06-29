package store

import (
	"context"
	"strconv"
	"testing"
)

// seedSess inserts a session with a chosen agent and machine under a user and
// project, bypassing ingest so the cross-project read paths can be asserted
// against known inputs. Rows are returned newest-id last.
func seedSess(t *testing.T, st *Store, userID, projectID int64, agent, machine, src string) int64 {
	t.Helper()
	var id int64
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		userID, projectID, agent, src, machine).Scan(&id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

// seedGlobalCorpus sets up one user, a remote and a local project, and six
// sessions: four claude / two codex, with one claude session carrying a blank
// machine to exercise the facet's blank exclusion. Returns the two project ids.
func seedGlobalCorpus(t *testing.T, st *Store) (userID, remoteID, localID int64) {
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

func TestListAllSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, remoteID, _ := seedGlobalCorpus(t, st)

	all, err := st.ListAllSessions(ctx, SessionFilter{})
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
	claude, err := st.ListAllSessions(ctx, SessionFilter{Agent: "claude"})
	if err != nil || len(claude) != 4 {
		t.Fatalf("agent filter: len=%d err=%v, want 4", len(claude), err)
	}
	inRemote, err := st.ListAllSessions(ctx, SessionFilter{ProjectID: remoteID})
	if err != nil || len(inRemote) != 3 {
		t.Fatalf("project filter: len=%d err=%v, want 3", len(inRemote), err)
	}
	byMachine, err := st.ListAllSessions(ctx, SessionFilter{Machine: "rig"})
	if err != nil || len(byMachine) != 2 {
		t.Fatalf("machine filter: len=%d err=%v, want 2", len(byMachine), err)
	}

	// Limit caps the page.
	capped, err := st.ListAllSessions(ctx, SessionFilter{Limit: 2})
	if err != nil || len(capped) != 2 {
		t.Fatalf("limit: len=%d err=%v, want 2", len(capped), err)
	}
}

// TestSessionFacetTrigger exercises the rollup trigger directly: an insert counts
// up, a re-attribution (project change) shifts the count, and a delete counts
// down and drops the emptied value.
func TestSessionFacetTrigger(t *testing.T) {
	st := newTestStore(t)
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
	st := newTestStore(t)
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

	// Projects: both projects with three sessions each.
	if len(f.Projects) != 2 {
		t.Fatalf("project facet = %+v, want 2", f.Projects)
	}
	for _, p := range f.Projects {
		if p.Count != 3 {
			t.Errorf("project %q count = %d, want 3", p.Key, p.Count)
		}
	}
}
