// Package guide is akari's self-hosted user guide: the Markdown chapters, the
// ordered chapter registry, and the rendering that turns them into the HTML the
// web layer shows and the plain-text forms an agent ingests.
//
// The chapters live as plain Markdown under content/ (embedded in the binary),
// each starting with its H1. Chapter metadata (title, summary, order, slug) is a
// typed registry here rather than frontmatter parsed out of the files, so the
// ordering and nav are compile-time data and the served .md stays clean,
// self-contained Markdown.
//
// Rendering is goldmark with GFM (tables and the like) and GitHub-style heading
// ids. A small AST pass rewrites the relative cross-links the chapters use
// (./glossary.md) to the hosted routes (/guide/glossary) for the HTML view,
// while the raw .md the agents fetch keeps the portable relative form. The same
// parse pass collects the H2/H3 headings the table of contents rail is built
// from.
package guide

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

//go:embed content/*.md
var content embed.FS

// Chapter is one page of the guide. The prose lives in the embedded Markdown
// file; the metadata here drives the sidebar, the table of contents, and the
// machine-readable index.
type Chapter struct {
	// Slug is the URL slug. The overview page uses "index" and renders at
	// /guide; every other chapter renders at /guide/<slug>.
	Slug string
	// Title matches the chapter's H1 and labels it in the nav and the index.
	Title string
	// Summary is the one-line description used for the page's meta description and
	// the llms.txt index. It does not render on the page itself.
	Summary string
	// Order sets reading order: 0 is the overview, 1..n the chapters. It is the
	// sole ordering key, so a chapter can be renamed without disturbing the
	// sequence.
	Order int
	// file is the embedded content filename.
	file string
}

// IsIndex reports whether this is the overview/index chapter (order 0), which
// renders at /guide rather than /guide/<slug>.
func (c Chapter) IsIndex() bool { return c.Order == 0 }

// Route is the HTML route the chapter renders at.
func (c Chapter) Route() string {
	if c.IsIndex() {
		return "/guide"
	}
	return "/guide/" + c.Slug
}

// RawRoute is the route serving the chapter's raw Markdown.
func (c Chapter) RawRoute() string {
	return "/guide/" + c.Slug + ".md"
}

// chapters is the ordered registry: the source of truth for what exists and in
// what order. Each title mirrors its chapter's H1; a test guards that they have
// not drifted from the files.
var chapters = []Chapter{
	{Slug: "index", Order: 0, file: "index.md",
		Title:   "akari user guide",
		Summary: "One searchable history of every AI coding-agent session across your fleet, self-hosted."},
	{Slug: "introduction", Order: 1, file: "introduction.md",
		Title:   "Introduction",
		Summary: "Why akari exists and the client/server model the rest of the system follows from."},
	{Slug: "getting-started", Order: 2, file: "getting-started.md",
		Title:   "Getting started",
		Summary: "Install the client, mint an ingest token, and push your first sessions."},
	{Slug: "the-client", Order: 3, file: "the-client.md",
		Title:   "The client",
		Summary: "The akari CLI in depth: login, sync, watch, the daemon, discovery, and the resumable upload."},
	{Slug: "the-web-ui", Order: 4, file: "the-web-ui.md",
		Title:   "The web UI",
		Summary: "Reading your history: the overview, the session feed, projects, and the transcript view."},
	{Slug: "accounts-and-sharing", Order: 5, file: "accounts-and-sharing.md",
		Title:   "Accounts and sharing",
		Summary: "Registration and invites, the three token scopes, and publishing a session or your usage overview."},
	{Slug: "agent-access", Order: 6, file: "agent-access.md",
		Title:   "Agent access",
		Summary: "Point a coding agent at your history through the read-only Model Context Protocol endpoint."},
	{Slug: "self-hosting", Order: 7, file: "self-hosting.md",
		Title:   "Self-hosting",
		Summary: "Run the server: Docker Compose, configuration, the database, the first admin, and reparse."},
	{Slug: "glossary", Order: 8, file: "glossary.md",
		Title:   "Glossary",
		Summary: "The terms the guide uses: sessions, projects, the fleet, transcripts, tokens and cost, and reparse."},
}

// Chapters returns the guide's chapters in reading order.
func Chapters() []Chapter {
	out := make([]Chapter, len(chapters))
	copy(out, chapters)
	return out
}

// Lookup returns the chapter for a slug, false if there is none. The empty slug
// and "index" both resolve to the overview, so /guide and /guide/index.md agree.
func Lookup(slug string) (Chapter, bool) {
	if slug == "" {
		slug = "index"
	}
	for _, c := range chapters {
		if c.Slug == slug {
			return c, true
		}
	}
	return Chapter{}, false
}

// Neighbors returns the previous and next chapters in reading order, either nil
// at an end of the chain. It powers the prev/next footer.
func (c Chapter) Neighbors() (prev, next *Chapter) {
	for i := range chapters {
		if chapters[i].Slug != c.Slug {
			continue
		}
		if i > 0 {
			prev = &chapters[i-1]
		}
		if i < len(chapters)-1 {
			next = &chapters[i+1]
		}
		return prev, next
	}
	return nil, nil
}

// Raw returns the chapter's Markdown source, trimmed with a single trailing
// newline. This is the exact text served at the .md route, copied by "Copy
// page", and concatenated into llms-full.txt.
func (c Chapter) Raw() (string, error) {
	b, err := content.ReadFile("content/" + c.file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)) + "\n", nil
}

// Heading is one entry in a chapter's table of contents: an H2 or H3 with the
// id its anchor targets.
type Heading struct {
	Level int
	ID    string
	Text  string
}

// Rendered is a chapter's HTML body plus the headings its table of contents is
// built from.
type Rendered struct {
	HTML     template.HTML
	Headings []Heading
}

// md is the shared goldmark instance. GFM covers the tables the reference pages
// use; WithAutoHeadingID gives every heading a GitHub-style id so the table of
// contents and in-page anchors resolve; the docLinks transformer rewrites the
// chapters' relative .md cross-links to the hosted routes for the HTML view.
var md = sync.OnceValue(func() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(docLinks{}, 100)),
		),
	)
})

// Render renders the chapter to HTML and collects its H2/H3 headings in one
// parse. A render failure is returned rather than swallowed: a broken chapter
// should surface as a server error, not a blank page.
func (c Chapter) Render() (Rendered, error) {
	raw, err := c.Raw()
	if err != nil {
		return Rendered{}, err
	}
	src := []byte(raw)
	doc := md().Parser().Parse(text.NewReader(src))

	var buf bytes.Buffer
	if err := md().Renderer().Render(&buf, src, doc); err != nil {
		return Rendered{}, err
	}

	headings := collectHeadings(doc, src)
	return Rendered{HTML: template.HTML(buf.String()), Headings: headings}, nil //nolint:gosec // content is our own trusted Markdown
}

// collectHeadings walks the parsed document for the H2 and H3 headings the
// table-of-contents rail lists (H1 is the page title; H4+ is noise), pairing
// each with the id WithAutoHeadingID assigned.
func collectHeadings(doc ast.Node, src []byte) []Heading {
	var out []Heading
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok || h.Level < 2 || h.Level > 3 {
			return ast.WalkContinue, nil
		}
		id, _ := h.AttributeString("id")
		idStr, _ := id.([]byte)
		out = append(out, Heading{
			Level: h.Level,
			ID:    string(idStr),
			Text:  string(h.Text(src)), //nolint:staticcheck // Text is adequate for plain heading text
		})
		return ast.WalkContinue, nil
	})
	return out
}

// docLinks rewrites the relative cross-links the chapters author (./glossary.md,
// getting-started.md#section) into the hosted routes (/guide/glossary,
// /guide/getting-started#section) so the rendered HTML navigates the served
// guide, while the raw .md keeps the portable relative form. Only internal
// .md targets are touched; external URLs, bare anchors, and other links pass
// through unchanged.
type docLinks struct{}

func (docLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		if rewritten, ok := rewriteDocLink(string(link.Destination)); ok {
			link.Destination = []byte(rewritten)
		}
		return ast.WalkContinue, nil
	})
}

// rewriteDocLink maps an internal chapter link to its hosted route, returning
// false for anything that is not one. It handles an optional ./ prefix and a
// trailing #anchor: index.md becomes /guide, other.md becomes /guide/other.
func rewriteDocLink(dest string) (string, bool) {
	// Leave absolute and scheme-qualified links (http:, mailto:, //host) alone.
	if dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "#") {
		return "", false
	}
	if i := strings.IndexByte(dest, ':'); i >= 0 {
		return "", false
	}
	path, anchor, _ := strings.Cut(dest, "#")
	path = strings.TrimPrefix(path, "./")
	if !strings.HasSuffix(path, ".md") {
		return "", false
	}
	slug := strings.TrimSuffix(path, ".md")
	var route string
	if slug == "index" {
		route = "/guide"
	} else {
		route = "/guide/" + slug
	}
	if anchor != "" {
		route += "#" + anchor
	}
	return route, true
}

// The base URL for the machine-readable index and the concatenated guide. The
// server passes its own external origin so the advertised links resolve.

// LLMsTxt renders the llms.txt discovery index (https://llmstxt.org): a short
// header and a link to every chapter's raw Markdown, so an agent learns the
// guide's shape in one fetch and can pull any page as Markdown. base is the
// server's external origin, no trailing slash.
func LLMsTxt(base string) string {
	var b strings.Builder
	b.WriteString("# akari\n\n")
	b.WriteString("> One searchable history of every AI coding-agent session across your fleet, self-hosted.\n\n")
	b.WriteString("akari collects the local session logs of Claude Code, Codex, and pi from every machine, parses them on one server, and shows them as a searchable history grouped by git project, with token usage and cost on every run. This is the user guide.\n\n")
	b.WriteString("## User guide\n\n")
	for _, c := range chapters {
		fmt.Fprintf(&b, "- [%s](%s%s): %s\n", c.Title, base, c.RawRoute(), c.Summary)
	}
	b.WriteString("\n## Optional\n\n")
	fmt.Fprintf(&b, "- [Full guide as one file](%s/llms-full.txt): every chapter concatenated for a single fetch.\n", base)
	fmt.Fprintf(&b, "- [Source repository](%s): the server, the client, and the engineering design.\n", repoURL)
	return b.String()
}

// LLMsFullTxt renders llms-full.txt: every chapter concatenated in reading
// order, each section prefixed with an HTML comment carrying its canonical URL
// so a fact can be cited back to a page. An agent ingests the whole guide in one
// request instead of crawling.
func LLMsFullTxt(base string) (string, error) {
	var sections []string
	for _, c := range chapters {
		raw, err := c.Raw()
		if err != nil {
			return "", err
		}
		url := base + c.Route()
		sections = append(sections, fmt.Sprintf("<!-- %s -->\n\n%s", url, strings.TrimSpace(raw)+"\n"))
	}
	header := "# akari user guide (full)\n\n> The complete user guide, concatenated in reading order. Canonical pages live under " + base + "/guide.\n\n"
	return header + strings.Join(sections, "\n---\n\n"), nil
}

// repoURL is the public source repository, used for the "Edit on GitHub" page
// action and the llms.txt source link.
const repoURL = "https://github.com/jssblck/akari"

// GitHubURL is the source location of a chapter's Markdown, for the "Edit on
// GitHub" page action.
func (c Chapter) GitHubURL() string {
	return repoURL + "/blob/main/internal/guide/content/" + c.file
}
