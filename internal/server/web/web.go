// Package web holds Akari's public server-rendered pages and their view helpers.
package web

import (
	"embed"
	"fmt"
	"net/http"
)

// errorTitle is the browser-tab title for a public error page: the status code
// paired with its standard reason ("404 Not Found"), so the tab and any shared
// link say what went wrong rather than a bare number. An unknown code with no
// standard text falls back to the number alone.
func errorTitle(code int) string {
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("%d", code)
}

// Static holds the embedded public assets served under /static/.
//
//go:embed static
var Static embed.FS

// OGMeta carries the Open Graph and Twitter card metadata a public page emits so a
// shared link unfurls with a title, description, and preview image. The zero value
// emits only the basic title tags; Image (an absolute URL) switches the Twitter
// card to the large-image form and adds og:image. Description and URL are optional.
type OGMeta struct {
	Title       string
	Description string
	// Image is the absolute URL of the preview card. Open Graph requires an absolute
	// URL here, so the handler builds it from the request origin; empty omits it.
	Image string
	// URL is the absolute canonical URL of the page; empty omits og:url.
	URL string
}
