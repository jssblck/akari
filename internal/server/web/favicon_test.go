package web

import (
	"io/fs"
	"strings"
	"testing"
)

// Both the signed-in shell and the public layout must declare the tab icon, and
// every asset they name (or the server serves at the root) must exist in the
// embedded FS. This guards the favicon the way TestStylesheetsLinkEmbeddedFiles
// guards the stylesheets: rename or drop an icon file without fixing the <link>
// (or the reverse) and this fails rather than shipping a 404ing tab icon.
func TestFaviconLinksEmbeddedAssets(t *testing.T) {
	wantLinks := []string{
		`<link rel="icon" href="/favicon.ico" sizes="32x32">`,
		`<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`,
		`<link rel="apple-touch-icon" href="/static/apple-touch-icon.png">`,
	}
	for _, tc := range []struct {
		name string
		html string
	}{
		{"app layout", renderComponent(t, LoginPage(Page{Title: "Log in"}, "/", ""))},
		{"public layout", renderComponent(t, PublicErrorPage(404, "gone"))},
	} {
		for _, want := range wantLinks {
			if !strings.Contains(tc.html, want) {
				t.Errorf("%s: missing favicon link %q", tc.name, want)
			}
		}
	}

	// The .ico is served at the root path, but its bytes live beside the other
	// icons in the embedded FS; the httpapi handler reads it from there.
	for _, path := range []string{"static/favicon.ico", "static/favicon.svg", "static/apple-touch-icon.png"} {
		if _, err := fs.Stat(Static, path); err != nil {
			t.Errorf("favicon asset %q is not in the embedded FS (%v)", path, err)
		}
	}
}
