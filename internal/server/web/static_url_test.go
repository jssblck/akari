package web

import (
	"strings"
	"testing"
)

func TestStaticURLFingerprintsEmbeddedAsset(t *testing.T) {
	got := string(StaticURL("js/insights.js"))
	if !strings.HasPrefix(got, "/static/js/insights.js?v=") {
		t.Fatalf("StaticURL() = %q, want fingerprinted insights URL", got)
	}
	if got != string(StaticURL("/js/insights.js")) {
		t.Fatal("StaticURL should accept an optional leading slash")
	}
}

func TestLayoutUsesFingerprintedChartRuntime(t *testing.T) {
	html := renderComponent(t, LoginPage(Page{Title: "Log in"}, "/", ""))
	for _, asset := range []string{"htmx.min.js", "charts.js", "app.js", "js/insights.js"} {
		want := `src="/static/` + asset + `?v=`
		if !strings.Contains(html, want) {
			t.Errorf("layout is missing fingerprinted runtime %q", asset)
		}
	}
}

func TestStaticURLRejectsMissingAsset(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("StaticURL should panic for an asset that cannot be served")
		}
	}()
	StaticURL("js/missing.js")
}
