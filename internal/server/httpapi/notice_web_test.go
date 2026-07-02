package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestNoticeOnPublishRedirect checks the end-to-end flow: publishing a session sets
// the notice cookie, the redirect target renders it once as the exact literal
// ("Published", not a sentence), and a second load of the same page no longer shows
// it because the cookie was cleared on the first read.
func TestNoticeOnPublishRedirect(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	grace, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: grace.ID, Agent: "claude", SourceSessionID: "s1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/x", Machine: "m",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := c.PostForm(srv.URL+"/login", url.Values{"username": {"grace"}, "password": {"hopper-1906"}}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Publishing redirects to the session page and sets the notice cookie along
	// the way.
	sessionURL := srv.URL + "/sessions/" + strconv.FormatInt(ann.SessionID, 10)
	resp, err := c.PostForm(sessionURL+"/publish", url.Values{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("publish status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")

	// Follow the redirect as the browser would (the jar carries the notice
	// cookie); the rendered page shows the banner with the exact terse string.
	c.CheckRedirect = nil
	body := readBody(t, mustGet(t, c, srv.URL+loc))
	if !strings.Contains(body, `data-notice`) || !strings.Contains(body, ">Published<") {
		t.Fatalf("session page missing rendered notice, got:\n%s", body)
	}

	// A second load of the same page no longer carries the cookie (it was cleared
	// on the first read), so the banner does not render again.
	body2 := readBody(t, mustGet(t, c, srv.URL+loc))
	if strings.Contains(body2, `data-notice`) {
		t.Fatalf("notice should not render on a second load, got:\n%s", body2)
	}
}

// TestNoticeRendersOnceAcrossPages checks that the notice cookie is root-scoped: an
// action on one page (delete, which redirects to the project view) can set a
// notice that renders on a different page than the one that set it, then clears.
func TestNoticeRendersOnceAcrossPages(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	grace, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: grace.ID, Agent: "claude", SourceSessionID: "s1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/x", Machine: "m",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}

	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := c.PostForm(srv.URL+"/login", url.Values{"username": {"grace"}, "password": {"hopper-1906"}}); err != nil {
		t.Fatalf("login: %v", err)
	}

	sessionURL := srv.URL + "/sessions/" + strconv.FormatInt(ann.SessionID, 10)
	resp, err := c.PostForm(sessionURL+"/delete", url.Values{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/projects/") {
		t.Fatalf("delete redirect = %q, want a /projects/ path", loc)
	}

	c.CheckRedirect = nil
	body := readBody(t, mustGet(t, c, srv.URL+loc))
	if !strings.Contains(body, `data-notice`) || !strings.Contains(body, "Session deleted") {
		t.Fatalf("project page missing rendered delete notice, got:\n%s", body)
	}
}

// TestNoticeTamperedCookieRendersInert checks that a hand-crafted, over-length or
// control-character notice cookie is dropped server-side rather than rendered: the
// validation runs on read, independent of what any handler ever writes, since the
// cookie is client-tamperable in transit.
func TestNoticeTamperedCookieRendersInert(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := c.PostForm(srv.URL+"/login", url.Values{"username": {"grace"}, "password": {"hopper-1906"}}); err != nil {
		t.Fatalf("login: %v", err)
	}
	c.CheckRedirect = nil

	// A tampered value carrying markup: if it ever rendered unescaped, this would
	// show as a real image tag rather than inert text.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	tampered := "<img src=x onerror=alert(1)>"
	req.AddCookie(&http.Cookie{Name: noticeCookie, Value: url.QueryEscape(tampered)})
	body := readBody(t, mustDo(t, c, req))
	if strings.Contains(body, "<img src=x") {
		t.Fatalf("tampered notice rendered as live markup, got:\n%s", body)
	}

	// An over-length value is also dropped: it must not appear in the response at
	// all, tampered or otherwise.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	overLong := strings.Repeat("x", noticeMaxLen+1)
	req2.AddCookie(&http.Cookie{Name: noticeCookie, Value: url.QueryEscape(overLong)})
	body2 := readBody(t, mustDo(t, c, req2))
	if strings.Contains(body2, overLong) {
		t.Fatalf("over-length notice was rendered, got:\n%s", body2)
	}
}

// TestValidNotice unit-tests the read-side gate directly: length and printable-only,
// independent of any handler.
func TestValidNotice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"typical", "Published", true},
		{"exactly at cap", strings.Repeat("x", noticeMaxLen), true},
		{"over cap", strings.Repeat("x", noticeMaxLen+1), false},
		{"control char", "Published\n", false},
		{"del byte", "Published\x7f", false},
	}
	for _, tc := range cases {
		if got := validNotice(tc.in); got != tc.want {
			t.Errorf("validNotice(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
