package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// seedTurnedSession announces a session for the owner and rebuilds it with n
// alternating user/assistant messages (a user turn every two ordinals), the shape
// the transcript window's turn-boundary math keys on.
func seedTurnedSession(t *testing.T, st *store.Store, ownerID, projectID int64, source string, n int) int64 {
	t.Helper()
	ann, err := st.Announce(context.Background(), store.AnnounceParams{
		UserID: ownerID, Agent: "claude", SourceSessionID: source,
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce %q: %v", source, err)
	}
	msgs := make([]store.MessageDelta, 0, n)
	for i := 0; i < n; i++ {
		role, content := "user", fmt.Sprintf("prompt %d", i)
		if i%2 == 1 {
			role, content = "assistant", fmt.Sprintf("reply %d", i)
		}
		msgs = append(msgs, store.MessageDelta{Ordinal: i, Role: role, Content: content})
	}
	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{Messages: msgs})
	return ann.SessionID
}

// TestSessionTranscriptFragments drives /sessions/{id}/body through its fragment
// shapes end to end against a session long enough to window: the full page opens on
// the tail window behind a "Show earlier" bar, ?before pages the preceding window in,
// ?after appends new rows with the out-of-band instrument swaps, and a cursor past
// the projection's end retargets to a whole-body re-render instead of appending into
// a DOM that no longer matches.
func TestSessionTranscriptFragments(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// 65 user turns, each answered: 130 messages. The tail window keeps the last 50
	// turns, so the page opens at ordinal 30 with 30 earlier messages behind the bar.
	sid := seedTurnedSession(t, st, owner.ID, projectID, "sess-window", 130)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		return resp, readBody(t, resp)
	}

	// The full page renders the instruments and only the tail window, with the
	// earlier bar naming its cursor and count.
	resp, body := get(fmt.Sprintf("/sessions/%d", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session page = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{`id="session-instruments"`, `id="transcript-earlier"`, "before=30", "30 earlier messages", `id="msg-30"`, `id="msg-129"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}
	if strings.Contains(body, `id="msg-29"`) {
		t.Fatal("session page rendered a message before the window start")
	}

	// ?before pages the preceding window in. Everything earlier fits in one window
	// here, so the fragment carries rows 0..29 and no replacement bar.
	resp, body = get(fmt.Sprintf("/sessions/%d/body?before=30", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?before = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{`id="msg-0"`, `id="msg-29"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("?before fragment missing %q", want)
		}
	}
	if strings.Contains(body, `id="transcript-earlier"`) {
		t.Fatal("?before fragment rendered a new earlier bar with nothing left to show")
	}

	// ?after appends only the rows past the cursor, plus the out-of-band swaps that
	// keep the instruments and subagents fold live.
	resp, body = get(fmt.Sprintf("/sessions/%d/body?after=127", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?after = %d, want 200", resp.StatusCode)
	}
	if h := resp.Header.Get("HX-Retarget"); h != "" {
		t.Fatalf("valid append should not retarget, got HX-Retarget=%q", h)
	}
	for _, want := range []string{
		`id="msg-128"`, `id="msg-129"`,
		`id="session-instruments"`, `id="session-subagents"`,
		`id="session-flow"`, `id="session-outline"`,
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("?after fragment missing %q", want)
		}
	}
	// The ribbon and outline out-of-band refreshes cover the whole session, so the
	// fragment names turns the transcript window itself does not carry.
	if !strings.Contains(body, `id="ol-0"`) {
		t.Fatal("?after fragment should refresh the outline across the whole session")
	}
	if strings.Contains(body, `id="msg-127"`) {
		t.Fatal("?after fragment re-sent the row the client already holds")
	}

	// A quiet tick (the cursor already at the live edge) appends nothing but still
	// carries the out-of-band instrument refreshes, and must not fall back to a
	// re-render: the SSE wake fires on raw bytes, which can precede the rebuild that
	// grows the projection. The whole-session shape (ribbon, outline) stays home: no
	// turns changed, so shipping it would make every quiet tick cost the session's
	// full outline.
	resp, body = get(fmt.Sprintf("/sessions/%d/body?after=129", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quiet ?after = %d, want 200", resp.StatusCode)
	}
	if h := resp.Header.Get("HX-Retarget"); h != "" {
		t.Fatalf("quiet tick should not retarget, got HX-Retarget=%q", h)
	}
	if strings.Contains(body, `id="msg-`) {
		t.Fatal("quiet tick should append no rows")
	}
	for _, want := range []string{`id="session-instruments"`, `id="session-subagents"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("quiet tick missing out-of-band refresh %q", want)
		}
	}
	for _, skip := range []string{`id="session-flow"`, `id="session-outline"`} {
		if strings.Contains(body, skip) {
			t.Fatalf("quiet tick should not ship the whole-session shape, found %q", skip)
		}
	}

	// A cursor naming an ordinal the projection does not have means the open tab's
	// DOM no longer matches (an epoch rebuild reshaped the transcript). The response
	// must retarget and re-render the windowed body whole rather than append.
	resp, body = get(fmt.Sprintf("/sessions/%d/body?after=4000", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale ?after = %d, want 200", resp.StatusCode)
	}
	if rt, rs := resp.Header.Get("HX-Retarget"), resp.Header.Get("HX-Reswap"); rt != "#session-body" || rs != "innerHTML" {
		t.Fatalf("stale cursor headers = (%q, %q), want (#session-body, innerHTML)", rt, rs)
	}
	for _, want := range []string{
		`id="transcript-earlier"`, `id="msg-129"`,
		// A reshaped projection renumbers ordinals, so the resync must also refresh
		// the shape surfaces outside #session-body out-of-band.
		`id="session-flow" hx-swap-oob="outerHTML"`,
		`id="session-outline" class="outline" aria-label="Session outline" hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stale-cursor re-render missing %q", want)
		}
	}
}

// TestSessionAppendEmptiedProjectionRetargets pins the resync on a projection a rebuild
// emptied: a browser still holding old rows sends its cursor, no such ordinal exists any
// longer, and the response must re-render the (now empty) windowed body rather than
// return an empty append that leaves the stale rows standing.
func TestSessionAppendEmptiedProjectionRetargets(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sid := seedTurnedSession(t, st, owner.ID, projectID, "sess-emptied", 20)
	// A rebuild that produces no rows (e.g. the raw was superseded) replaces the
	// projection with nothing.
	rebuildWith(t, st, sid, store.ProjectionDelta{})

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	resp, err := c.Get(srv.URL + fmt.Sprintf("/sessions/%d/body?after=5", sid))
	if err != nil {
		t.Fatalf("get ?after over emptied projection: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?after = %d, want 200", resp.StatusCode)
	}
	if rt, rs := resp.Header.Get("HX-Retarget"), resp.Header.Get("HX-Reswap"); rt != "#session-body" || rs != "innerHTML" {
		t.Fatalf("emptied projection headers = (%q, %q), want (#session-body, innerHTML)", rt, rs)
	}
	if strings.Contains(body, `id="msg-`) {
		t.Fatal("emptied projection re-render should hold no rows")
	}
	if !strings.Contains(body, "No messages parsed yet.") {
		t.Fatal("emptied projection re-render should show the empty state")
	}
}

// TestSessionAppendCapRetargets pins the other resync trigger: when more rows landed
// than one append fragment may carry, the handler re-renders the windowed body whole
// instead of streaming an unbounded fragment.
func TestSessionAppendCapRetargets(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// 700 messages: over the 600-message fragment cap from a cursor at the start.
	sid := seedTurnedSession(t, st, owner.ID, projectID, "sess-cap", 700)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	resp, err := c.Get(srv.URL + fmt.Sprintf("/sessions/%d/body?after=0", sid))
	if err != nil {
		t.Fatalf("get ?after=0: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?after=0 = %d, want 200", resp.StatusCode)
	}
	if rt, rs := resp.Header.Get("HX-Retarget"), resp.Header.Get("HX-Reswap"); rt != "#session-body" || rs != "innerHTML" {
		t.Fatalf("capped append headers = (%q, %q), want (#session-body, innerHTML)", rt, rs)
	}
	// The re-render is the tail window, not the whole run: the last 50 of 350 turns.
	if !strings.Contains(body, `id="msg-699"`) || !strings.Contains(body, `id="transcript-earlier"`) {
		t.Fatal("capped append should re-render the windowed body")
	}
	if strings.Contains(body, `id="msg-0"`) {
		t.Fatal("capped append re-render should stay windowed, not stream the whole run")
	}
}
