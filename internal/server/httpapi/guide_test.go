package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The user guide is public: an anonymous visitor reaches every surface without a
// redirect to login. This drives the HTML page, the raw-Markdown mirror, and the
// two llms endpoints against a real Routes() handler.
func TestGuideRoutesArePublic(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	// A client that does NOT follow redirects, so a bounce to /login shows up as a
	// 303 rather than being silently followed.
	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// The index renders the docs layout with the chapter rail, logged out.
	resp := mustGet(t, c, srv.URL+"/guide")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /guide = %d, want 200 (the guide is public)", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{
		`class="guide-nav"`,
		`akari user guide`,
		`href="/static/guide.css"`,
		// The logged-out corner action is Log in, but the page itself renders (no
		// redirect to the login form).
		`href="/login"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET /guide missing %q, got:\n%s", want, body)
		}
	}

	// A chapter renders and its relative cross-links are rewritten to hosted routes.
	resp = mustGet(t, c, srv.URL+"/guide/glossary")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /guide/glossary = %d, want 200", resp.StatusCode)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Definitions for the terms the rest of the guide uses") {
		t.Fatalf("chapter body missing its intro, got:\n%s", body)
	}
	if !strings.Contains(body, `href="/guide/accounts-and-sharing`) {
		t.Fatalf("chapter should carry a rewritten internal link, got:\n%s", body)
	}
	// The prose must carry no un-rewritten relative cross-link. (The head's
	// rel=alternate and the "View as Markdown" action legitimately end in .md, so
	// the tell is the ./ relative form, which only an un-rewritten link would use.)
	if strings.Contains(body, `href="./`) {
		t.Fatalf("rendered chapter must not carry a relative ./ link, got:\n%s", body)
	}

	// The raw-Markdown mirror serves text/markdown, starts with the H1, and keeps
	// the portable relative links (the .md form agents and Copy page consume).
	resp = mustGet(t, c, srv.URL+"/guide/glossary.md")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /guide/glossary.md = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("raw markdown content-type = %q, want text/markdown", ct)
	}
	raw := readBody(t, resp)
	if !strings.HasPrefix(raw, "# Glossary\n") {
		t.Fatalf("raw markdown should start with the H1, got:\n%s", raw[:min(80, len(raw))])
	}
	if !strings.Contains(raw, "](./accounts-and-sharing.md)") {
		t.Fatalf("raw markdown should keep relative .md links, got:\n%s", raw)
	}

	// An unknown chapter is a 404, not a redirect.
	resp = mustGet(t, c, srv.URL+"/guide/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /guide/does-not-exist = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// llms.txt is the discovery index: text/plain, listing every chapter's raw
	// Markdown and the full file.
	resp = mustGet(t, c, srv.URL+"/llms.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /llms.txt = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("llms.txt content-type = %q, want text/plain", ct)
	}
	llms := readBody(t, resp)
	for _, want := range []string{
		"# akari",
		"/guide/introduction.md",
		"/guide/self-hosting.md",
		"/llms-full.txt",
	} {
		if !strings.Contains(llms, want) {
			t.Fatalf("llms.txt missing %q, got:\n%s", want, llms)
		}
	}

	// llms-full.txt concatenates the whole guide in one fetch, each section under
	// its canonical URL comment.
	resp = mustGet(t, c, srv.URL+"/llms-full.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /llms-full.txt = %d, want 200", resp.StatusCode)
	}
	full := readBody(t, resp)
	for _, want := range []string{
		"# akari user guide (full)",
		"<!-- " + srv.URL + "/guide -->",
		"<!-- " + srv.URL + "/guide/self-hosting -->",
		"# Introduction",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("llms-full.txt missing %q", want)
		}
	}
}

// The signed-in app shell links the guide, so a reader can reach the docs from
// the sidebar.
func TestSidebarLinksGuide(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)

	if _, err := c.PostForm(srv.URL+"/register", map[string][]string{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	body := readBody(t, mustGet(t, c, srv.URL+"/overview"))
	if !strings.Contains(body, `href="/guide"`) {
		t.Fatalf("signed-in sidebar should link the guide, got:\n%s", body)
	}
}
