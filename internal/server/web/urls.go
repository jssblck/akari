package web

import (
	"fmt"

	"github.com/a-h/templ"
)

// ProjectHref and friends return sanitized internal URLs for href attributes.
func ProjectHref(id int64) templ.SafeURL { return templ.URL(fmt.Sprintf("/projects/%d", id)) }
func SessionHref(id int64) templ.SafeURL { return templ.URL(fmt.Sprintf("/sessions/%d", id)) }
func PublicHref(publicID string) templ.SafeURL {
	return templ.URL("/s/" + publicID)
}

// ProjectPath returns the plain string path, used for htmx attributes (which are
// not URL-sanitized like href).
func ProjectPath(id int64) string { return fmt.Sprintf("/projects/%d", id) }

// SSEPath and BodyPath are the live-update endpoints for a session, carried on
// data attributes for the static app.js to wire up.
func SSEPath(id int64) string  { return fmt.Sprintf("/sessions/%d/events", id) }
func BodyPath(id int64) string { return fmt.Sprintf("/sessions/%d/body", id) }

// Facets are the distinct filter values available for a project's session list.
type Facets struct {
	Agents   []string
	Machines []string
	Users    []string
}

// ChipLabel renders a tool body's media type and size, for example "36.0 KB json".
func ChipLabel(mediaType string, bytes int64) string {
	short := "data"
	switch mediaType {
	case "application/json":
		short = "json"
	case "text/plain":
		short = "text"
	case "":
		short = "data"
	default:
		short = mediaType
	}
	return FmtBytes(bytes) + " " + short
}

// StatusClass maps a tool result status to a CSS class.
func StatusClass(status string) string {
	if status == "error" {
		return "err"
	}
	return "ok"
}
