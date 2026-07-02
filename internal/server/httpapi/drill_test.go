package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
)

// registerFirstUser registers the first account (admin, no invite) through the browser
// flow, leaving the cookie-carrying client authenticated for the authed UI. It reuses the
// register form the other web tests drive.
func registerFirstUser(t *testing.T, srv, username, password string, c *http.Client) {
	t.Helper()
	resp, err := c.PostForm(srv+"/register", url.Values{
		"username": {username}, "password": {password},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
}

// TestSessionsDrillParsingAndChips exercises the drill-through query parsing and chip
// rendering on /sessions: a grade, outcome, and range param each parse into the filter and
// render as a removable chip, a malformed value is a 400, and a bare list carries no chips.
func TestSessionsDrillParsingAndChips(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	registerFirstUser(t, srv.URL, "grace", "hopper-1906", c)
	ctx := context.Background()

	// Seed a graded, completed session so the grade/outcome drills land on a real row and
	// the list renders rather than showing the empty state, though the chips render either
	// way. The signal row is current-version and non-stale, matching the filter's cohort.
	u, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sid int64
	now := time.Now()
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		        message_count, user_message_count, started_at, ended_at, updated_at, signals_stale)
		 VALUES ($1,$2,'claude','drill-1','box',10,3,$3,$4,$4,false) RETURNING id`,
		u.ID, pid, now.Add(-time.Hour), now.Add(-30*time.Minute)).Scan(&sid); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, grade)
		 VALUES ($1,$2,'completed','high','A')`, sid, quality.Version); err != nil {
		t.Fatalf("seed signal: %v", err)
	}

	// A grade drill parses and renders the grade chip, holding a removal link.
	body := readBody(t, mustGet(t, c, srv.URL+"/sessions?grade=A"))
	if !strings.Contains(body, ">grade<") || !strings.Contains(body, ">A<") {
		t.Errorf("grade=A should render a grade chip labelled A, got:\n%s", body)
	}

	// The unscored sentinel is accepted and rendered as the grade chip value.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions?grade=unscored"))
	if !strings.Contains(body, ">grade<") || !strings.Contains(body, ">unscored<") {
		t.Errorf("grade=unscored should render a grade chip labelled unscored, got:\n%s", body)
	}

	// An outcome drill parses and renders the outcome chip.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions?outcome=abandoned"))
	if !strings.Contains(body, ">outcome<") || !strings.Contains(body, ">abandoned<") {
		t.Errorf("outcome=abandoned should render an outcome chip, got:\n%s", body)
	}

	// A range drill parses and renders the range chip with the selector's own wording.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions?range=30d"))
	if !strings.Contains(body, ">range<") || !strings.Contains(body, ">30 days<") {
		t.Errorf("range=30d should render a range chip labelled '30 days', got:\n%s", body)
	}

	// A bare list carries no drill chips.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions"))
	if strings.Contains(body, ">grade<") || strings.Contains(body, ">outcome<") || strings.Contains(body, ">range<") {
		t.Errorf("bare /sessions should carry no drill chips, got:\n%s", body)
	}
}

// TestSessionsDrillMalformed pins the whitelist gate: an unknown grade or outcome is a 400,
// matching the malformed-project-filter precedent, rather than a silent unfiltered list.
func TestSessionsDrillMalformed(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)
	registerFirstUser(t, srv.URL, "grace", "hopper-1906", c)

	for _, tc := range []struct {
		name, path string
	}{
		{"bad grade", "/sessions?grade=Z"},
		{"bad outcome", "/sessions?outcome=finished"},
	} {
		resp := mustGet(t, c, srv.URL+tc.path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %s = %d, want 400", tc.name, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// An unknown range key normalizes to the default rather than erroring (the sort-key
	// precedent), so it is a 200, not a 400.
	resp := mustGet(t, c, srv.URL+"/sessions?range=decade")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("range=decade should normalize to default (200), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSessionsDrillCountAgreement confirms the handler's footer count agrees with the
// rendered rows under a grade filter, the "N of M" invariant CountAllSessions shares with
// the list through conds(). It seeds two graded sessions (one A, one F) and checks the A
// drill shows "1 of 1".
func TestSessionsDrillCountAgreement(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	registerFirstUser(t, srv.URL, "grace", "hopper-1906", c)
	ctx := context.Background()

	u, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	now := time.Now()
	seed := func(src, grade string) {
		var sid int64
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
			        message_count, user_message_count, started_at, ended_at, updated_at, signals_stale)
			 VALUES ($1,$2,'claude',$3,'box',10,3,$4,$5,$5,false) RETURNING id`,
			u.ID, pid, src, now.Add(-time.Hour), now.Add(-30*time.Minute)).Scan(&sid); err != nil {
			t.Fatalf("seed session %s: %v", src, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, grade)
			 VALUES ($1,$2,'completed','high',$3)`, sid, quality.Version, grade); err != nil {
			t.Fatalf("seed signal %s: %v", src, err)
		}
	}
	seed("drill-a", "A")
	seed("drill-f", "F")

	body := readBody(t, mustGet(t, c, srv.URL+"/sessions?grade=A"))
	if !strings.Contains(body, "1 of 1") {
		t.Errorf("grade=A footer should read '1 of 1' (one A session of one matching), got:\n%s", body)
	}
}
