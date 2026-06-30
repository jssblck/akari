// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// Static holds the embedded static assets (htmx, stylesheet) served under
// /static/.
//
//go:embed static
var Static embed.FS

// Page is the shared layout context for every rendered page.
type Page struct {
	Title    string
	Username string
	IsAdmin  bool
	LoggedIn bool
	// Active is the sidebar nav key for the current page ("overview",
	// "sessions", "projects", "account"), so the shell can mark the
	// current section. Empty leaves no item active.
	Active string
}

// IsLocalKind reports whether a project kind is one of the non-remote kinds
// (standalone or orphaned), which are grouped and labeled apart from git-remote
// projects in the UI.
func IsLocalKind(kind string) bool {
	return kind == "standalone" || kind == "orphaned"
}

// ProjectTitle is the heading shown for a project. A remote project shows its
// canonical remote key; a standalone or orphaned project shows its folder name,
// since its synthetic key ("local:machine:path") is an internal detail.
func ProjectTitle(p store.ProjectSummary) string {
	if IsLocalKind(p.Kind) {
		return p.DisplayName
	}
	return p.RemoteKey
}

// SessionProjectLabel is the project name shown in a session header: the folder
// name for a local session, the remote key otherwise. It keeps the synthetic
// "local:machine:path" key out of the heading.
func SessionProjectLabel(d store.SessionDetail) string {
	if IsLocalKind(d.ProjectKind) {
		return d.ProjectName
	}
	return d.ProjectKey
}

// SessionRowProject is the project label shown beside a session in the global
// session list: the folder name for a local project, the remote key otherwise.
func SessionRowProject(r store.SessionRow) string {
	if IsLocalKind(r.ProjectKind) {
		return r.ProjectName
	}
	return r.ProjectKey
}

// ProjectFacetLabel is the label for a project option in the session filter
// rail, friendly for a local project and the remote key otherwise.
func ProjectFacetLabel(pf store.ProjectFacet) string {
	if IsLocalKind(pf.Kind) {
		return pf.Name
	}
	return pf.Key
}

// LocalPath recovers the working-directory path from a local project's synthetic
// key ("local:machine:path"), for display beside the folder name. It returns ""
// for a remote project.
func LocalPath(p store.ProjectSummary) string {
	if !IsLocalKind(p.Kind) {
		return ""
	}
	return strings.TrimPrefix(p.RemoteKey, "local:"+p.Host+":")
}

// ToolsByOrdinal groups tool calls by the message ordinal they belong to, so the
// session view can render a message's tool calls beneath it.
func ToolsByOrdinal(tools []store.ToolCallView) map[int][]store.ToolCallView {
	m := map[int][]store.ToolCallView{}
	for _, t := range tools {
		m[t.MessageOrdinal] = append(m[t.MessageOrdinal], t)
	}
	return m
}

// DuplicateIDsLabel is the chip text for a session that repeats tool-call ids,
// pluralized for the count. The count itself is a bounded scalar from the store
// (DuplicateCallUIDCount), not computed over the in-memory tool calls.
func DuplicateIDsLabel(n int) string {
	if n == 1 {
		return "1 duplicate id"
	}
	return fmt.Sprintf("%d duplicate ids", n)
}

// AttachmentsByOrdinal groups attachments by the message ordinal they belong to,
// so the session view can render a message's images beneath it.
func AttachmentsByOrdinal(atts []store.AttachmentView) map[int][]store.AttachmentView {
	m := map[int][]store.AttachmentView{}
	for _, a := range atts {
		m[a.MessageOrdinal] = append(m[a.MessageOrdinal], a)
	}
	return m
}

// IsImageMedia reports whether a media type is an image the transcript can show
// inline, so a non-image attachment falls back to a download link instead.
func IsImageMedia(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/")
}

// AttachmentAlt is the alt text for a rendered image: its filename when one was
// recovered, else a generic label so the element is never unlabelled.
func AttachmentAlt(a store.AttachmentView) string {
	if a.Filename != "" {
		return a.Filename
	}
	return "attachment"
}

// FmtBytes renders a byte count compactly (the tool-body metadata chips).
func FmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FmtCost renders a USD cost. Sub-cent costs still show enough precision to be
// meaningful.
func FmtCost(usd float64, incomplete bool) string {
	var s string
	switch {
	case usd == 0:
		s = "$0"
	case usd < 0.01:
		s = fmt.Sprintf("$%.4f", usd)
	default:
		s = fmt.Sprintf("$%.2f", usd)
	}
	if incomplete {
		s += "+"
	}
	return s
}

// FmtTokens renders a token count with thousands separators.
func FmtTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// FmtTokensCompact renders a token count to a short magnitude (1.7M, 63.0k, 412),
// for the feed's inline figure where the exact value lives in the hover card. The
// thousands-separated FmtTokens stays the form for places that show the full
// number.
func FmtTokensCompact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FmtTime renders a timestamp, or a dash when absent.
func FmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// RowTokens is a session's total token volume across all four classes (input,
// output, cache read, cache write), matching the overview heatmap's notion of a
// day's "total tokens" so the figure and its breakdown agree across views.
func RowTokens(s store.SessionSummary) int64 {
	return s.TotalInput + s.TotalOutput + s.TotalCacheRead + s.TotalCacheWrite
}

// plural returns the "s" suffix for a count, so a label reads "1 session" but
// "2 sessions" without each call site repeating the conditional.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// FmtRelTime renders a timestamp as a coarse "time ago" for the recent past
// (today, 1 day ago, ...), falling back to an absolute stamp once it is a week
// or more old, where a relative phrasing stops being useful. It reads "now" from
// the wall clock; relTime holds the testable core. It backs the "Updated" column
// on both the projects index and the per-project session table, so the two read
// alike (the global feed groups by day instead and uses FeedTime).
func FmtRelTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return relTime(time.Now(), *t)
}

// relTime is FmtRelTime's clock-injected core. Day distance is measured between
// UTC calendar dates (not 24-hour windows), so a session from late last night
// reads "1 day ago" rather than "today" merely because fewer than 24 hours have
// passed.
func relTime(now, t time.Time) string {
	now, t = now.UTC(), t.UTC()
	nd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	td := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	days := int(nd.Sub(td).Hours() / 24)
	switch {
	case days <= 0: // today, or a future stamp from clock skew
		return "today"
	case days == 1:
		return "1 day ago"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("2006-01-02 15:04")
	}
}

// FmtTimeAt renders a non-pointer timestamp, or a dash when zero.
func FmtTimeAt(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// FmtDuration renders the span between start and end, or a dash.
func FmtDuration(start, end *time.Time) string {
	if start == nil || end == nil || start.IsZero() || end.IsZero() {
		return "-"
	}
	d := end.Sub(*start)
	if d < 0 {
		return "-"
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// BaseName returns the last path segment of a file path (handling both / and \
// separators), for a compact label in the outline. It returns the input
// unchanged when there is no separator.
func BaseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

// ReparseView is the reparse status the account page renders: whether one is
// running and how far along. The httpapi layer fills it from the reparse service,
// so the web package stays free of a dependency on that service.
type ReparseView struct {
	InProgress bool
	Done       int
	Total      int
	Failed     int
}

// ReparsePercent is the completed fraction of a reparse as an integer percent,
// for the progress bar's fill width. It is 0 before the total is known and is
// clamped to 100, so a late count revision can never overflow the track.
func ReparsePercent(done, total int) int {
	if total <= 0 {
		return 0
	}
	pct := done * 100 / total
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// navClass returns the sidebar link's class, adding "active" when its key is the
// page's current section.
func navClass(key, active string) string {
	if key == active {
		return "nav active"
	}
	return "nav"
}

// RoleClass maps a message role to a CSS class for styling.
func RoleClass(role string) string {
	switch role {
	case "user":
		return "msg-user"
	case "assistant":
		return "msg-assistant"
	default:
		return "msg-other"
	}
}
