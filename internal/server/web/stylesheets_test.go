package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// linkRe pulls the href out of every <link rel="stylesheet"> the layout emits.
var linkRe = regexp.MustCompile(`<link rel="stylesheet" href="(/static/[^"]+)">`)

// The stylesheet was split into per-area files served from static/css. Every
// file the layout links must exist in the embedded FS, or a page would 404 its
// styles. This guards the split: rename a css file without updating the <link>
// (or the reverse) and this fails. base.css must come first so its tokens and
// resets cascade under the per-page files.
func TestStylesheetsLinkEmbeddedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		html string
	}{
		{"app layout", renderComponent(t, LoginPage(Page{Title: "Log in"}, "/", ""))},
		{"public layout", renderComponent(t, PublicErrorPage(404, "gone"))},
	} {
		hrefs := linkRe.FindAllStringSubmatch(tc.html, -1)
		if len(hrefs) == 0 {
			t.Fatalf("%s: rendered no stylesheet links", tc.name)
		}
		if first := hrefs[0][1]; first != "/static/css/base.css" {
			t.Errorf("%s: first stylesheet is %q, want /static/css/base.css (tokens and resets must cascade first)", tc.name, first)
		}
		for _, h := range hrefs {
			href := h[1]
			embedPath := "static/" + strings.TrimPrefix(href, "/static/")
			if _, err := fs.Stat(Static, embedPath); err != nil {
				t.Errorf("%s: linked stylesheet %q is not in the embedded FS (%v)", tc.name, href, err)
			}
		}
	}
}

// Every css file embedded under static/css should be linked by the layout;
// otherwise it is dead weight in the binary. Pairs with the test above to make
// the link list and the file set agree in both directions.
func TestEmbeddedStylesheetsAreLinked(t *testing.T) {
	html := renderComponent(t, LoginPage(Page{Title: "Log in"}, "/", ""))
	linked := map[string]bool{}
	for _, h := range linkRe.FindAllStringSubmatch(html, -1) {
		linked[strings.TrimPrefix(h[1], "/static/")] = true
	}
	entries, err := fs.ReadDir(Static, "static/css")
	if err != nil {
		t.Fatalf("read static/css: %v", err)
	}
	for _, e := range entries {
		rel := "css/" + e.Name()
		if !linked[rel] {
			t.Errorf("embedded stylesheet %q is not linked by the layout", rel)
		}
	}
}
