package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// seedWindowedSession provisions an owner, logs the given client in as them, and
// builds one session whose transcript overflows the transcript window by extra
// messages (ordinals 0..window+extra-1, alternating user/assistant), so the page
// must window it. Returns the session id.
func seedWindowedSession(t *testing.T, srv string, st *store.Store, c *http.Client, extra int) int64 {
	t.Helper()
	ctx := context.Background()
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-window",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	total := web.TranscriptWindowSize + extra
	var msgs []store.MessageDelta
	for i := 0; i < total; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, store.MessageDelta{Ordinal: i, Role: role, Content: fmt.Sprintf("turn %d content", i)})
	}
	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{Messages: msgs})
	if _, err := c.PostForm(srv+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	return ann.SessionID
}

// get fetches a path with the logged-in client and returns the body, failing the
// test on a non-200.
func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body:\n%s", u, resp.StatusCode, body)
	}
	return body
}

// TestSessionPageWindowsTranscript pins the P-2 initial render: a long transcript's
// page carries every turn in the outline but only the most recent window of rows in
// the DOM, topped by a "Show earlier" bar keyed to the first rendered ordinal.
func TestSessionPageWindowsTranscript(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	sid := seedWindowedSession(t, srv.URL, st, c, 20)
	first := 20 // 120 messages, a 100-message window: ordinals 20..119 render

	body := get(t, c, srv.URL+fmt.Sprintf("/sessions/%d", sid))

	// The transcript holds exactly the tail window.
	for _, want := range []string{
		fmt.Sprintf(`id="msg-%d"`, first),
		`id="msg-119"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing windowed row %s", want)
		}
	}
	if strings.Contains(body, fmt.Sprintf(`id="msg-%d"`, first-1)) {
		t.Errorf("page rendered row %d, which is above the window", first-1)
	}

	// The outline still lists every turn, including the ones with no row in the DOM.
	for _, want := range []string{`id="ol-0"`, `id="ol-119"`} {
		if !strings.Contains(body, want) {
			t.Errorf("outline missing entry %s", want)
		}
	}

	// The "Show earlier" bar keys the previous window on the first rendered ordinal
	// and names the remainder.
	if !strings.Contains(body, `id="transcript-earlier"`) {
		t.Fatalf("page missing the show-earlier bar")
	}
	if !strings.Contains(body, fmt.Sprintf("?before=%d", first)) {
		t.Errorf("show-earlier bar does not key on the first rendered ordinal %d", first)
	}
	if !strings.Contains(body, "20 earlier messages") {
		t.Errorf("show-earlier bar does not name the 20 remaining messages")
	}
}

// TestSessionBodyEarlierFragment pins the ?before= fragment: only the previous
// window's rows, a fresh bar only while messages remain, and the &until= form
// returning the whole gap for the outline's fetch-then-scroll.
func TestSessionBodyEarlierFragment(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	sid := seedWindowedSession(t, srv.URL, st, c, 20)

	// Everything above the initial window fits one fetch: rows 0..19, and the bar is
	// gone because the head is reached.
	body := get(t, c, srv.URL+fmt.Sprintf("/sessions/%d/body?before=20", sid))
	for _, want := range []string{`id="msg-0"`, `id="msg-19"`} {
		if !strings.Contains(body, want) {
			t.Errorf("earlier fragment missing row %s", want)
		}
	}
	if strings.Contains(body, `id="msg-20"`) {
		t.Errorf("earlier fragment must exclude the boundary ordinal itself")
	}
	if strings.Contains(body, `id="transcript-earlier"`) {
		t.Errorf("earlier fragment at the head must not render another bar")
	}
	// The fragment is rows only: no stat band, no out-of-band swaps.
	if strings.Contains(body, "hx-swap-oob") || strings.Contains(body, `id="session-stats"`) {
		t.Errorf("earlier fragment must not carry OOB swaps, got:\n%s", body)
	}

	// The until= form (the outline jump) returns the half-open gap [until, before)
	// and a fresh bar keyed to the gap's first ordinal.
	body = get(t, c, srv.URL+fmt.Sprintf("/sessions/%d/body?before=110&until=105", sid))
	for _, want := range []string{`id="msg-105"`, `id="msg-109"`} {
		if !strings.Contains(body, want) {
			t.Errorf("gap fragment missing row %s", want)
		}
	}
	for _, reject := range []string{`id="msg-104"`, `id="msg-110"`} {
		if strings.Contains(body, reject) {
			t.Errorf("gap fragment rendered %s, outside [105, 110)", reject)
		}
	}
	if !strings.Contains(body, `id="transcript-earlier"`) || !strings.Contains(body, "?before=105") {
		t.Errorf("gap fragment must carry a fresh bar keyed on ordinal 105")
	}
	if !strings.Contains(body, "105 earlier messages") {
		t.Errorf("gap fragment bar must name the 105 remaining messages")
	}
}

// TestSessionBodyAfterFragment pins the live-append fragment: only the rows past
// the client's cursor, plus out-of-band swaps for the stat band and subagents.
func TestSessionBodyAfterFragment(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	sid := seedWindowedSession(t, srv.URL, st, c, 20)

	body := get(t, c, srv.URL+fmt.Sprintf("/sessions/%d/body?after=117", sid))
	for _, want := range []string{`id="msg-118"`, `id="msg-119"`} {
		if !strings.Contains(body, want) {
			t.Errorf("append fragment missing row %s", want)
		}
	}
	if strings.Contains(body, `id="msg-117"`) {
		t.Errorf("append fragment must exclude the cursor ordinal itself")
	}
	// The stat band and the subagents wrapper ride along as out-of-band swaps, so the
	// client's beforeend append also refreshes both without re-rendering the transcript.
	if !strings.Contains(body, `id="session-stats"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("append fragment missing the OOB stat band, got:\n%s", body)
	}
	if !strings.Contains(body, `id="session-subagents"`) {
		t.Errorf("append fragment missing the OOB subagents wrapper")
	}

	// Caught up: no rows, but the OOB stat band still refreshes.
	body = get(t, c, srv.URL+fmt.Sprintf("/sessions/%d/body?after=119", sid))
	if strings.Contains(body, `id="msg-`) {
		t.Errorf("caught-up append fragment must carry no rows, got:\n%s", body)
	}
	if !strings.Contains(body, `id="session-stats"`) {
		t.Errorf("caught-up append fragment must still refresh the stat band")
	}
}

// TestSessionBodyParamValidation pins that malformed cursors are a 400, not a 500
// or a silent full render.
func TestSessionBodyParamValidation(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	sid := seedWindowedSession(t, srv.URL, st, c, 1)

	for _, q := range []string{"?after=abc", "?before=abc", "?before=5&until=abc"} {
		resp, err := c.Get(srv.URL + fmt.Sprintf("/sessions/%d/body%s", sid, q))
		if err != nil {
			t.Fatalf("GET %s: %v", q, err)
		}
		readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", q, resp.StatusCode)
		}
	}
}
