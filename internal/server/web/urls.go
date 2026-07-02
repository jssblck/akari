package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/jssblck/akari/internal/server/store"
)

// OverviewPath builds the overview URL for a range key and a set of selected user
// ids: the shared target the range buttons link to and the user filter submits, so
// the two controls always round-trip the full window-and-users state together
// rather than each clobbering the other's selection.
func OverviewPath(rng string, userIDs []int64) string {
	q := url.Values{}
	if rng != "" {
		q.Set("range", rng)
	}
	for _, id := range userIDs {
		q.Add("user", strconv.FormatInt(id, 10))
	}
	if s := q.Encode(); s != "" {
		return "/?" + s
	}
	return "/"
}

// userValues encodes selected user ids as repeated ?user= params for the range
// selector to preserve, so switching the overview's window holds the chosen users.
// It mirrors what OverviewPath emits, fed through RangeOptions' generic preserve.
func userValues(userIDs []int64) url.Values {
	if len(userIDs) == 0 {
		return nil
	}
	v := make(url.Values, 1)
	for _, id := range userIDs {
		v.Add("user", strconv.FormatInt(id, 10))
	}
	return v
}

// SelectedUserIDs parses the overview's repeated ?user= ids against the known
// accounts, keeping only ids that name a real user and returning them in the
// users-list order. A tampered, stale, or non-numeric id silently drops out, and
// the stable order keeps the collapsed pills from reshuffling between requests.
func SelectedUserIDs(raw []string, users []store.User) []int64 {
	if len(raw) == 0 {
		return nil
	}
	want := map[int64]bool{}
	for _, v := range raw {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	var out []int64
	for _, u := range users {
		if want[u.ID] {
			out = append(out, u.ID)
		}
	}
	return out
}

// selectedSet indexes the selected ids so the account rows can test membership in
// O(1) while rendering. Marking checkboxes then stays linear in the user count
// rather than O(users * selected), which matters because the menu lists every
// account and the selection can grow to that same set.
func selectedSet(selected []int64) map[int64]bool {
	if len(selected) == 0 {
		return nil
	}
	m := make(map[int64]bool, len(selected))
	for _, id := range selected {
		m[id] = true
	}
	return m
}

// selectedUsers resolves the selected ids back to their accounts, in users-list
// order, so the collapsed control can render one pill per chosen user.
func selectedUsers(users []store.User, selected []int64) []store.User {
	if len(selected) == 0 {
		return nil
	}
	want := map[int64]bool{}
	for _, id := range selected {
		want[id] = true
	}
	var out []store.User
	for _, u := range users {
		if want[u.ID] {
			out = append(out, u)
		}
	}
	return out
}

// ProjectHref and friends return sanitized internal URLs for href attributes.
func ProjectHref(id int64) templ.SafeURL { return templ.URL(fmt.Sprintf("/projects/%d", id)) }
func SessionHref(id int64) templ.SafeURL { return templ.URL(fmt.Sprintf("/sessions/%d", id)) }
func PublicHref(publicID string) templ.SafeURL {
	return templ.URL("/s/" + publicID)
}

// ProjectPath returns the plain string path, used for htmx attributes (which are
// not URL-sanitized like href).
func ProjectPath(id int64) string { return fmt.Sprintf("/projects/%d", id) }

// SessionPath is the plain-string session path, used for the row-navigation data
// attribute that makes a whole table row a click target.
func SessionPath(id int64) string { return fmt.Sprintf("/sessions/%d", id) }

// SessionsBasePath is the global (cross-project) session list.
const SessionsBasePath = "/sessions"

// validOutcomes and validGrades are the trust boundaries for the two signals filters:
// only a value present here reaches SessionFilter, so a tampered or stale ?outcome / ?grade
// falls through to "" (no filter) rather than into the query. They mirror the Insights
// distribution buckets exactly (the four outcomes, the five letters, plus the "unscored"
// sentinel for the empty grade bucket), so every drill-down link the page emits round-trips.
var validOutcomes = map[string]bool{"completed": true, "abandoned": true, "errored": true, "unknown": true}
var validGrades = map[string]bool{"A": true, "B": true, "C": true, "D": true, "F": true, "unscored": true}

// ValidOutcome returns v when it names a real outcome bucket, else "" (no filter). It is the
// handler's guard on the ?outcome param, the counterpart to store.IsSortKey for the sort param.
func ValidOutcome(v string) string {
	if validOutcomes[v] {
		return v
	}
	return ""
}

// ValidGrade returns v when it names a real grade bucket (a letter or the "unscored"
// sentinel), else "" (no filter). It is the handler's guard on the ?grade param.
func ValidGrade(v string) string {
	if validGrades[v] {
		return v
	}
	return ""
}

// sessionsQuery builds the query string for the global session list from a filter and an active
// range key, omitting empty fields, so facet links and the htmx swap target agree. rng is threaded
// separately rather than reverse-engineered from the filter's Since (a time carries no key), so a
// drill-down link and the toolbar's chip round-trip the same ?range value the handler parsed.
//
// rng is encoded only when it names a bounded window (a known key that is not "all"): the sessions
// feed's natural form is all-history, so an "all" or empty range adds no param and the bare
// /sessions path stays unbounded, matching how the default order omits its sort param.
func sessionsQuery(f store.SessionFilter, rng string) string {
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
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	if f.Grade != "" {
		q.Set("grade", f.Grade)
	}
	if f.ProjectID != 0 {
		q.Set("project", fmt.Sprintf("%d", f.ProjectID))
	}
	if RangeBounds(rng) {
		q.Set("range", rng)
	}
	// The default order (most recent first) is the bare URL; any other column or
	// direction is encoded so the sort link round-trips and survives a reload.
	if !isDefaultOrder(f) {
		q.Set("sort", effSort(f))
		if f.Desc {
			q.Set("dir", "desc")
		} else {
			q.Set("dir", "asc")
		}
	}
	if s := q.Encode(); s != "" {
		return "?" + s
	}
	return ""
}

// effSort resolves a filter's effective sort column, treating the empty string
// as the default so the templates can read one canonical key.
func effSort(f store.SessionFilter) string {
	if f.Sort == "" {
		return store.DefaultSort
	}
	return f.Sort
}

// isDefaultOrder reports whether a filter carries the global list's default order
// (most recent first), which the URL omits. A zero-value filter counts as the
// default: its empty Sort has never been narrowed, so the bare /sessions path is
// the natural form even though its Desc is the zero false.
func isDefaultOrder(f store.SessionFilter) bool {
	return f.Sort == "" || (f.Sort == store.DefaultSort && f.Desc)
}

// SessionsPath is the full global session-list path for the current selection and active range,
// used as the htmx swap target so a facet click updates the URL coherently. rng is the active
// range key (7d/30d/90d/year/all); it is encoded only for a bounded window, so the unscoped feed
// stays the bare /sessions path.
func SessionsPath(f store.SessionFilter, rng string) string {
	return SessionsBasePath + sessionsQuery(f, rng)
}

// SessionsHref is the sanitized href form of SessionsPath, for anchor tags.
func SessionsHref(f store.SessionFilter, rng string) templ.SafeURL {
	return templ.URL(SessionsPath(f, rng))
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
	case "outcome":
		if f.Outcome == value {
			f.Outcome = ""
		} else {
			f.Outcome = value
		}
	case "grade":
		if f.Grade == value {
			f.Grade = ""
		} else {
			f.Grade = value
		}
	}
	return f
}

// AgentFacetHref and friends return the toggle href for a facet option, holding the rest of the
// current selection and the active range so removing one filter does not drop the window.
func AgentFacetHref(f store.SessionFilter, value, rng string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "agent", value), rng)
}
func UserFacetHref(f store.SessionFilter, value, rng string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "user", value), rng)
}
func MachineFacetHref(f store.SessionFilter, value, rng string) templ.SafeURL {
	return SessionsHref(facetToggle(f, "machine", value), rng)
}

// ProjectFacetHref toggles the project selection for a facet row, holding the
// rest of the current selection and the active range.
func ProjectFacetHref(f store.SessionFilter, id int64, rng string) templ.SafeURL {
	if f.ProjectID == id {
		f.ProjectID = 0
	} else {
		f.ProjectID = id
	}
	return SessionsHref(f, rng)
}

// facetHref dispatches a text facet's toggle link to the right field helper, carrying the range.
func facetHref(field, value string, f store.SessionFilter, rng string) templ.SafeURL {
	switch field {
	case "agent":
		return AgentFacetHref(f, value, rng)
	case "user":
		return UserFacetHref(f, value, rng)
	case "machine":
		return MachineFacetHref(f, value, rng)
	case "outcome":
		return SessionsHref(facetToggle(f, "outcome", value), rng)
	case "grade":
		return SessionsHref(facetToggle(f, "grade", value), rng)
	}
	return SessionsHref(f, rng)
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
	return f.Agent != "" || f.Username != "" || f.Machine != "" || f.ProjectID != 0 ||
		f.Outcome != "" || f.Grade != ""
}

// PublicPath is the plain-string public URL, shown to the owner as the shareable
// link to copy.
func PublicPath(publicID string) string { return "/s/" + publicID }

// PublicOverviewPath is the plain-string path of a user's public usage overview,
// rooted at /u/<username>. The username is path-escaped so an unusual character
// cannot break the URL or escape the segment. The range selector on the public
// page builds its buttons from this base (via RangeOptions), so switching the
// window refetches the public path rather than the authed overview, and the
// account page shows it as the shareable link.
func PublicOverviewPath(username string) string { return "/u/" + url.PathEscape(username) }

// PublicOverviewHref is the sanitized href form of PublicOverviewPath, for the
// account page's link and the signed-in overview badge.
func PublicOverviewHref(username string) templ.SafeURL {
	return templ.URL(PublicOverviewPath(username))
}

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
