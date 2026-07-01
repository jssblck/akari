package guide

import (
	"strings"
	"testing"
)

// norm collapses runs of whitespace to single spaces, so a line-wrapped
// blockquote compares equal to its one-line summary.
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

// subtitle extracts the first blockquote (the chapter subtitle) after the H1,
// with its "> " markers stripped and whitespace collapsed.
func subtitle(raw string) string {
	lines := strings.Split(raw, "\n")
	var parts []string
	inQuote := false
	for _, ln := range lines[1:] { // skip the H1
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, ">") {
			inQuote = true
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(t, ">")))
			continue
		}
		if inQuote {
			break
		}
	}
	return norm(strings.Join(parts, " "))
}

// The registry must be strictly ordered starting at 0 (the index), with unique
// slugs, so the sidebar, prev/next, and llms outputs all read in one stable
// order.
func TestChaptersAreOrdered(t *testing.T) {
	cs := Chapters()
	if len(cs) == 0 {
		t.Fatal("no chapters")
	}
	if cs[0].Order != 0 || !cs[0].IsIndex() {
		t.Fatalf("first chapter must be the index (order 0), got %+v", cs[0])
	}
	seen := map[string]bool{}
	for i, c := range cs {
		if c.Order != i {
			t.Errorf("chapter %q has order %d, want %d (registry must be dense and in order)", c.Slug, c.Order, i)
		}
		if seen[c.Slug] {
			t.Errorf("duplicate slug %q", c.Slug)
		}
		seen[c.Slug] = true
		if c.Title == "" || c.Summary == "" || c.file == "" {
			t.Errorf("chapter %q missing metadata: %+v", c.Slug, c)
		}
	}
}

// Every chapter must render, and its registry title must match its H1 and its
// summary its subtitle blockquote, so the nav and index never drift from the
// prose. This also proves each embedded file is present and parseable.
func TestEveryChapterRendersAndMatchesMetadata(t *testing.T) {
	for _, c := range Chapters() {
		raw, err := c.Raw()
		if err != nil {
			t.Errorf("%s: raw: %v", c.Slug, err)
			continue
		}
		if !strings.HasSuffix(raw, "\n") || strings.HasSuffix(raw, "\n\n") {
			t.Errorf("%s: raw must end in exactly one trailing newline", c.Slug)
		}
		if strings.HasPrefix(raw, "---") {
			t.Errorf("%s: raw markdown must not carry YAML frontmatter", c.Slug)
		}
		wantH1 := "# " + c.Title
		if !strings.HasPrefix(raw, wantH1+"\n") {
			t.Errorf("%s: first line is not %q (registry title drifted from the H1)", c.Slug, wantH1)
		}
		if got, want := subtitle(raw), norm(c.Summary); got != want {
			t.Errorf("%s: subtitle blockquote %q does not match registry summary %q", c.Slug, got, want)
		}
		r, err := c.Render()
		if err != nil {
			t.Errorf("%s: render: %v", c.Slug, err)
			continue
		}
		if strings.TrimSpace(string(r.HTML)) == "" {
			t.Errorf("%s: rendered empty HTML", c.Slug)
		}
	}
}

// The concepts chapter has H2 sections, so its table of contents must be
// populated with anchored H2/H3 headings the rail can link.
func TestHeadingsExtracted(t *testing.T) {
	c, ok := Lookup("concepts")
	if !ok {
		t.Fatal("concepts chapter missing")
	}
	r, err := c.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(r.Headings) < 3 {
		t.Fatalf("expected several headings, got %d", len(r.Headings))
	}
	for _, h := range r.Headings {
		if h.Level < 2 || h.Level > 3 {
			t.Errorf("heading %q has level %d, want 2 or 3", h.Text, h.Level)
		}
		if h.ID == "" || h.Text == "" {
			t.Errorf("heading missing id or text: %+v", h)
		}
	}
}

// Rendered HTML must rewrite the relative .md cross-links to hosted routes, so
// the browsing reader navigates the served guide rather than hitting dead
// ./file.md links.
func TestRenderRewritesInternalLinks(t *testing.T) {
	c, _ := Lookup("index")
	r, err := c.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := string(r.HTML)
	if strings.Contains(html, `href="./`) || strings.Contains(html, `.md"`) {
		t.Errorf("rendered HTML still carries a relative .md link:\n%s", html)
	}
	if !strings.Contains(html, `href="/guide/introduction"`) {
		t.Errorf("expected a rewritten link to /guide/introduction in the index")
	}
}

func TestRewriteDocLink(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"./concepts.md", "/guide/concepts", true},
		{"concepts.md", "/guide/concepts", true},
		{"./index.md", "/guide", true},
		{"./getting-started.md#push", "/guide/getting-started#push", true},
		{"#anchor", "", false},
		{"https://example.com/x.md", "", false},
		{"mailto:me@example.com", "", false},
		{"../other.md", "/guide/../other", true}, // still rewritten; authors use plain ./slug.md
		{"not-markdown", "", false},
	}
	for _, tc := range cases {
		got, ok := rewriteDocLink(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("rewriteDocLink(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// llms.txt must list every chapter by title with a link to its raw .md, and
// point at the full file, so an agent can discover and fetch the whole guide.
func TestLLMsTxt(t *testing.T) {
	out := LLMsTxt("https://akari.example.com")
	if !strings.HasPrefix(out, "# akari\n") {
		t.Errorf("llms.txt should start with the akari header")
	}
	for _, c := range Chapters() {
		link := "https://akari.example.com" + c.RawRoute()
		if !strings.Contains(out, link) {
			t.Errorf("llms.txt missing chapter link %q", link)
		}
		if !strings.Contains(out, c.Title) || !strings.Contains(out, c.Summary) {
			t.Errorf("llms.txt missing title or summary for %q", c.Slug)
		}
	}
	if !strings.Contains(out, "https://akari.example.com/llms-full.txt") {
		t.Errorf("llms.txt should point at llms-full.txt")
	}
}

// llms-full.txt must carry every chapter's body in order, each prefixed with its
// canonical URL comment, so a single fetch loads the whole guide with citable
// section boundaries.
func TestLLMsFullTxt(t *testing.T) {
	out, err := LLMsFullTxt("https://akari.example.com")
	if err != nil {
		t.Fatalf("LLMsFullTxt: %v", err)
	}
	if !strings.HasPrefix(out, "# akari user guide (full)\n") {
		t.Errorf("llms-full.txt should start with the full-guide header")
	}
	for _, c := range Chapters() {
		marker := "<!-- https://akari.example.com" + c.Route() + " -->"
		if !strings.Contains(out, marker) {
			t.Errorf("llms-full.txt missing section marker %q", marker)
		}
	}
	// The index route in the marker is /guide, not /guide/index.
	if strings.Contains(out, "/guide/index -->") {
		t.Errorf("index section should be marked /guide, not /guide/index")
	}
}

func TestLookup(t *testing.T) {
	if c, ok := Lookup(""); !ok || !c.IsIndex() {
		t.Errorf(`Lookup("") should resolve to the index`)
	}
	if c, ok := Lookup("index"); !ok || c.Slug != "index" {
		t.Errorf(`Lookup("index") should resolve to the index`)
	}
	if _, ok := Lookup("does-not-exist"); ok {
		t.Errorf("unknown slug should not resolve")
	}
}

func TestNeighbors(t *testing.T) {
	first, _ := Lookup("index")
	if prev, next := first.Neighbors(); prev != nil || next == nil {
		t.Errorf("index should have no prev and a next")
	}
	last, _ := Lookup("self-hosting")
	if prev, next := last.Neighbors(); prev == nil || next != nil {
		t.Errorf("last chapter should have a prev and no next")
	}
}

// The house style forbids em and en dashes in authored prose; the guide is
// prose we author, so guard that none crept in.
func TestNoDashesInContent(t *testing.T) {
	for _, c := range Chapters() {
		raw, err := c.Raw()
		if err != nil {
			t.Fatal(err)
		}
		if i := strings.IndexAny(raw, "—–"); i >= 0 {
			start := i - 30
			if start < 0 {
				start = 0
			}
			t.Errorf("%s: contains an em/en dash near: %q", c.Slug, raw[start:i+1])
		}
	}
}
