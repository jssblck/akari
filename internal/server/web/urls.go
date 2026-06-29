package web

import (
	"fmt"
	"net/url"

	"github.com/a-h/templ"

	"github.com/jssblck/akari/internal/server/store"
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

// SessionsBasePath is the global (cross-project) session list.
const SessionsBasePath = "/sessions"

// sessionsQuery builds the query string for the global session list from a
// filter, omitting empty fields, so facet links and the htmx swap target agree.
func sessionsQuery(f store.SessionFilter) string {
	q := url.Values{}
	if f.Agent != "" {
		q.Set("agent", f.Agent)
	}
	if f.Username != "" {
		q.Set("user", f.Username)
	}
	if f.Machine != "" {
		q.Set("machine", f.Machine)
	}
	if f.ProjectID != 0 {
		q.Set("project", fmt.Sprintf("%d", f.ProjectID))
	}
	if s := q.Encode(); s != "" {
		return "?" + s
	}
	return ""
}

// SessionsPath is the full global session-list path for the current selection,
// used as the htmx swap target so a facet click updates the URL coherently.
func SessionsPath(f store.SessionFilter) string {
	return SessionsBasePath + sessionsQuery(f)
}

// SessionsHref is the sanitized href form of SessionsPath, for anchor tags.
func SessionsHref(f store.SessionFilter) templ.SafeURL {
	return templ.URL(SessionsPath(f))
}

// facetToggle returns a copy of the filter with one field set to value, or
// cleared when value already equals the current selection (so clicking an active
// facet removes it). It powers the toggle behavior of the facet rail.
func facetToggle(f store.SessionFilter, field, value string) store.SessionFilter {
	switch field {
	case "agent":
		if f.Agent == value {
			f.Agent = ""
		} else {
			f.Agent = value
		}
	case "user":
		if f.Username == value {
			f.Username = ""
		} else {
			f.Username = value
		}
	case "machine":
		if f.Machine == value {
			f.Machine = ""
		} else {
			f.Machine = value
		}
	}
	return f
}

// AgentFacetHref and friends return the toggle href for a facet option, holding
// the rest of the current selection.
func AgentFacetHref(f store.SessionFilter, value string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "agent", value))
}
func UserFacetHref(f store.SessionFilter, value string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "user", value))
}
func MachineFacetHref(f store.SessionFilter, value string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "machine", value))
}

// ProjectFacetHref toggles the project selection for a facet row, holding the
// rest of the current selection.
func ProjectFacetHref(f store.SessionFilter, id int64) templ.SafeURL {
	if f.ProjectID == id {
		f.ProjectID = 0
	} else {
		f.ProjectID = id
	}
	return SessionsHref(f)
}

// facetHref dispatches a text facet's toggle link to the right field helper.
func facetHref(field, value string, f store.SessionFilter) templ.SafeURL {
	switch field {
	case "agent":
		return AgentFacetHref(f, value)
	case "user":
		return UserFacetHref(f, value)
	case "machine":
		return MachineFacetHref(f, value)
	}
	return SessionsHref(f)
}

// facetOptClass marks a text facet option active when it is the current
// selection for its field.
func facetOptClass(field, value string, f store.SessionFilter) string {
	active := (field == "agent" && f.Agent == value) ||
		(field == "user" && f.Username == value) ||
		(field == "machine" && f.Machine == value)
	if active {
		return "opt active"
	}
	return "opt"
}

// projectOptClass marks the selected project option active.
func projectOptClass(id int64, f store.SessionFilter) string {
	if f.ProjectID == id {
		return "opt active"
	}
	return "opt"
}

// swatchClass maps a categorical index to its viz-ramp swatch class (viz-0..7),
// matching VizColor's ordering so a project's rail swatch and any chart series
// agree.
func swatchClass(i int) string {
	return fmt.Sprintf("swatch viz-%d", i%8)
}

// projectLabelByID finds a project facet's display label by id, for the active
// filter chip. It falls back to the numeric id if the project is not in the set.
func projectLabelByID(opts []store.ProjectFacet, id int64) string {
	for _, o := range opts {
		if o.ID == id {
			return ProjectFacetLabel(o)
		}
	}
	return fmt.Sprintf("#%d", id)
}

// AnyFilterActive reports whether the global session list is currently narrowed,
// so the view can show a "clear all" affordance only when it would do something.
func AnyFilterActive(f store.SessionFilter) bool {
	return f.Agent != "" || f.Username != "" || f.Machine != "" || f.ProjectID != 0
}

// PublicPath is the plain-string public URL, shown to the owner as the shareable
// link to copy.
func PublicPath(publicID string) string { return "/s/" + publicID }

// SessionBlobBase and PublicBlobBase are the per-session prefixes under which CAS
// blobs are served, for the authenticated and logged-out views respectively. A
// blob URL is the base plus "/blob/{sha256}"; serving is gated on the session
// referencing the hash.
func SessionBlobBase(id int64) string       { return fmt.Sprintf("/api/v1/session/%d", id) }
func PublicBlobBase(publicID string) string { return "/s/" + publicID }

// BlobURL builds a tool body's fetch URL from a session blob base and a hash.
func BlobURL(base, sha string) string { return base + "/blob/" + sha }

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
