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
// rather than each clobbering the other's selection. It is rooted at /overview
// (the app home), not "/" (the public homepage).
func OverviewPath(rng string, userIDs []int64) string {
	q := url.Values{}
	if rng != "" {
		q.Set("range", rng)
	}
	for _, id := range userIDs {
		q.Add("user", strconv.FormatInt(id, 10))
	}
	if s := q.Encode(); s != "" {
		return "/overview?" + s
	}
	return "/overview"
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
// the stable order keeps the collapsed chips from reshuffling between requests.
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
// order, so the collapsed control can render one chip per chosen user.
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
	if f.Query != "" {
		q.Set("q", f.Query)
	}
	// Grade, outcome, and range arrive from an Insights drill-through link and ride the
	// URL so a chip-removal or "Show more" swap round-trips them, exactly like the other
	// facets. Range is the window key that produced Since (Since itself is not URL-serialized).
	if f.Grade != "" {
		q.Set("grade", f.Grade)
	}
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	if f.Range != "" {
		q.Set("range", f.Range)
	}
	// Empty sessions are hidden by default, so the flag rides the URL only when the
	// reader has opted to show them, keeping the bare path the common case.
	if f.IncludeEmpty {
		q.Set("empty", "1")
	}
	// Subagents are hidden by default, so the flag rides the URL only when the reader
	// has opted to include them, so a facet click or "Show more" holds that choice.
	if f.IncludeSubagents {
		q.Set("subagents", "1")
	}
	// The span constraint rides the URL only when set (the busiest-user drill), so the
	// linked feed round-trips the same spanned cohort the concurrency panel counted.
	if f.RequireSpan {
		q.Set("spanned", "1")
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

// DefaultSessionLimit is the global feed's page size, the fixed slice each request reads.
// "Show more" no longer grows this: it passes a keyset cursor and appends the next page of
// the same size (see ShowMorePath), so depth is unbounded and the page cost stays flat.
const DefaultSessionLimit = 100

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
// The content search counts: it is a removable narrowing like the facets, and its
// chip clears alongside them.
func AnyFilterActive(f store.SessionFilter) bool {
	return f.Agent != "" || f.Username != "" || f.Machine != "" || f.ProjectID != 0 || f.Query != "" ||
		f.Grade != "" || f.Outcome != "" || f.Range != ""
}

// GradeClearHref, OutcomeClearHref, and RangeClearHref are the removal links for the
// grade, outcome, and range chips: each drops just its own param while holding every
// other facet, search, sort, and window, matching the agent/user chip behavior.
func GradeClearHref(f store.SessionFilter) templ.SafeURL {
	f.Grade = ""
	return SessionsHref(f)
}

func OutcomeClearHref(f store.SessionFilter) templ.SafeURL {
	f.Outcome = ""
	return SessionsHref(f)
}

func RangeClearHref(f store.SessionFilter) templ.SafeURL {
	f.Range = ""
	return SessionsHref(f)
}

// RangeChipLabel is the active-filter chip value for the window, reusing the range
// selector's own option wording ("30 days", "Year") so the chip reads the same as the
// button that could have set it. It falls back to the raw key for an unknown value,
// though the handler validates the key before it reaches here.
func RangeChipLabel(key string) string {
	for _, dr := range DateRanges {
		if dr.Key == key {
			return dr.Label
		}
	}
	return key
}

// SearchClearHref is the toggle link for the active search chip: it drops the query
// while holding every other facet, sort, and the empty toggle, so removing a search
// leaves the rest of the narrowing in place. It returns the feed to its first page
// implicitly: the base path carries no keyset cursor (only "Show more" appends one), so
// a cleared-search link always reopens at the top.
func SearchClearHref(f store.SessionFilter) templ.SafeURL {
	f.Query = ""
	return SessionsHref(f)
}

// EmptyToggleHref flips the include-empty state for the footer's toggle, holding every
// other facet, search, and sort. Like every filter link it carries no keyset cursor, so
// flipping the empty state reopens the feed at the first page rather than resuming mid-walk
// under the other visibility, which the changed row set would make meaningless.
func EmptyToggleHref(f store.SessionFilter) templ.SafeURL {
	f.IncludeEmpty = !f.IncludeEmpty
	return SessionsHref(f)
}

// ShowMorePath is the plain-string path the "Show more" button fetches: the current
// feed's filter plus a keyset cursor (afterID, the last visible row's id, and afterVal, that
// row's sort value as the page saw it) so the store resumes strictly after it rather than
// re-reading the page under a doubled limit. afterVal fixes the resume boundary to what the
// reader already saw, so a later change to the cursor row's own column cannot drift the page
// and duplicate or skip rows; it is empty for an order with no keyset cursor. afterDay is the
// last row's day-bucket key, carried only for the day-grouped default order so the appended
// page suppresses a repeated heading; count is the running total already shown, so the appended
// footer reports the cumulative "Showing N" without counting the corpus; maxTok is the feed's
// token-bar denominator (the first page's largest session), carried so the appended page scales
// its bars against the same reference rather than its own page maximum. The cursor rides only
// this link, never the facet or sort URLs, so any filter change resets to the first page.
func ShowMorePath(f store.SessionFilter, afterID int64, afterVal, afterDay string, count int, maxTok int64) string {
	base := SessionsPath(f)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	q := url.Values{}
	q.Set("after", fmt.Sprintf("%d", afterID))
	if afterVal != "" {
		q.Set("av", afterVal)
	}
	if afterDay != "" {
		q.Set("after_day", afterDay)
	}
	if count > 0 {
		q.Set("count", fmt.Sprintf("%d", count))
	}
	if maxTok > 0 {
		q.Set("maxtok", fmt.Sprintf("%d", maxTok))
	}
	return base + sep + q.Encode()
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

// PublicOverviewOGPath is the path of the Open Graph preview card for a user's
// published overview, the /u/<username>/og.png the page advertises as og:image and
// the route serves the rendered PNG from. It is PublicOverviewPath with the card
// suffix, so the page tag and the route stay one definition rather than two string
// literals that could drift.
func PublicOverviewOGPath(username string) string { return PublicOverviewPath(username) + "/og.png" }

// PublicProjectPath is the plain-string path of a project's public usage overview,
// rooted at /p/<id>. The range selector on the public page builds its buttons from
// this base (via RangeOptions), so switching the window refetches the public path
// rather than the authed project page, and the signed-in project page shows it as
// the shareable link.
func PublicProjectPath(id int64) string { return fmt.Sprintf("/p/%d", id) }

// PublicProjectHref is the sanitized href form of PublicProjectPath, for the
// project page's share link and public badge.
func PublicProjectHref(id int64) templ.SafeURL {
	return templ.URL(PublicProjectPath(id))
}

// PublicProjectOGPath is the path of the Open Graph preview card for a project's
// published overview, the /p/<id>/og.png the page advertises as og:image and the
// route serves the rendered PNG from. Built off PublicProjectPath so the tag and the
// route share one definition.
func PublicProjectOGPath(id int64) string { return PublicProjectPath(id) + "/og.png" }

// PublicSessionOGPath is the path of the Open Graph preview card for a published
// session, the /s/<public_id>/og.png the page advertises as og:image and the route
// serves the rendered PNG from. Built off PublicPath so the tag and the route share
// one definition.
func PublicSessionOGPath(publicID string) string { return PublicPath(publicID) + "/og.png" }

// ProjectPublishPath and ProjectUnpublishPath are the POST targets for the project
// page's publicity control, mirroring the account overview toggles. They are plain
// strings the templ form actions wrap in templ.SafeURL.
func ProjectPublishPath(id int64) string   { return fmt.Sprintf("/projects/%d/overview/publish", id) }
func ProjectUnpublishPath(id int64) string { return fmt.Sprintf("/projects/%d/overview/unpublish", id) }

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
