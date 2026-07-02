// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// locCtxKey keys the viewer's timezone in the render context. The httpapi layer
// resolves it from the tz cookie and stashes it before rendering; the formatting
// helpers below read it back through Loc. An unexported type keeps the key from
// colliding with any other package's context values.
type locCtxKey struct{}

// WithLoc returns a context carrying the viewer's timezone, for the httpapi render
// path to attach before a component renders. A nil location is ignored so the
// accessor's UTC default still applies.
func WithLoc(ctx context.Context, loc *time.Location) context.Context {
	if loc == nil {
		return ctx
	}
	return context.WithValue(ctx, locCtxKey{}, loc)
}

// Loc is the viewer's timezone for the current render, or UTC when none was set.
// The formatting helpers localize every stamp and day heading through it, so a
// reader sees times in their own zone; a page rendered outside a request (or
// before the cookie is set) reads UTC. templ exposes the render ctx implicitly, so
// a template calls FmtTime(ctx, t) and this reads the zone off that ctx.
func Loc(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(locCtxKey{}).(*time.Location); ok && loc != nil {
		return loc
	}
	return time.UTC
}

// noticeCtxKey keys the one-shot notice banner text in the render context, the
// same seam Loc uses: the httpapi layer drains the notice cookie once, at the
// render seam every page shares, and stashes the text here rather than
// threading it through every pageFor/pageForNav call site. An unexported type
// keeps the key from colliding with any other package's context values.
type noticeCtxKey struct{}

// WithNotice returns a context carrying a one-shot notice banner's text, for the
// httpapi render path to attach before a component renders. An empty string is
// ignored, so the accessor's no-notice default still applies.
func WithNotice(ctx context.Context, notice string) context.Context {
	if notice == "" {
		return ctx
	}
	return context.WithValue(ctx, noticeCtxKey{}, notice)
}

// Notice is the current render's one-shot notice banner text, or "" when none was
// set. The authed layout renders it once at the top of main when non-empty.
func Notice(ctx context.Context) string {
	n, _ := ctx.Value(noticeCtxKey{}).(string)
	return n
}

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
	// OverviewPublic reports whether the signed-in user has published their own
	// usage overview, which drives the account page's Publicity controls and the
	// "Public" badge on the signed-in overview. The share link is /u/<username>, so
	// Username (already on the page) is all the badge and account section need to
	// build it. This is populated from the same UserByID lookup that fills Username,
	// so reading it costs no extra query.
	OverviewPublic bool
}

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

// FmtPercent renders a 0..1 fraction as a whole-number percent, for the cache hit
// rate. A real but tiny rate (under 1%) rounds up to 1% rather than 0%, so a scope
// that did hit the cache never reads as a total miss; a true zero stays 0%.
func FmtPercent(f float64) string {
	if f <= 0 {
		return "0%"
	}
	p := f * 100
	if p < 1 {
		return "1%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

// FmtSavings renders a cache saving for the Cache tile. A non-negative saving reads as
// "saved $X"; the rare negative, where cache was written but never re-read enough to
// repay the creation premium, reads as "cost $X" on its magnitude, so the figure stays
// honest without printing a minus sign into a "saved" label.
//
// An incomplete saving reads "... partial", NOT the "$X+" lower-bound marker the cost
// figures use. A saving omitted for an unpriced model can be negative (a Claude cache
// write is priced above input), so the true figure could be lower OR higher than shown:
// "partial" says it is incomplete without implying a direction the data cannot support.
func FmtSavings(usd float64, incomplete bool) string {
	verb := "saved "
	if usd < 0 {
		verb, usd = "cost ", -usd
	}
	s := verb + FmtCost(usd, false)
	if incomplete {
		s += " partial"
	}
	return s
}

// HeaderStats bundles the derived stat-tile inputs the session instrument header
// renders beside the token figures: prompt-cache effectiveness and the session's
// quality signals. Threading one struct keeps the SessionMain / sessionStats / public
// render seams stable as the header grows, rather than widening every signature each
// time a new tile lands.
type HeaderStats struct {
	Cache   store.CacheStats
	Signals store.SessionSignals
}

// QualityGrade is the headline of the session Quality tile: the letter grade for a
// scored session, or a plain dash for an unscored one (an unknown outcome with no tool
// signal, where a letter would invent a verdict the transcript does not support).
func QualityGrade(s store.SessionSignals) string {
	if s.Scored() {
		return *s.Grade
	}
	return "-"
}

// QualityGradeClass is the CSS modifier for the Quality tile, banding its colour the
// way a report card reads: A and B good, C watch, D and F poor, an unscored session
// neutral. It maps to the status palette already in the sheet (sage, peach, rose)
// rather than introducing new hues, keeping to the One Voice Rule.
func QualityGradeClass(s store.SessionSignals) string {
	if !s.Scored() {
		return "q-none"
	}
	switch *s.Grade {
	case "A", "B":
		return "q-good"
	case "C":
		return "q-watch"
	default: // D, F
		return "q-poor"
	}
}

// OutcomeLabel renders a stored outcome string title-cased for display. An empty or
// unrecognized value reads "Unknown", so the tile never shows a blank cell.
func OutcomeLabel(outcome string) string {
	switch outcome {
	case "completed":
		return "Completed"
	case "abandoned":
		return "Abandoned"
	case "errored":
		return "Errored"
	default:
		return "Unknown"
	}
}

// QualityScoreLabel renders the numeric score for the Quality tooltip: "n / 100" for a
// scored session, "not scored" otherwise, so an unscored session reads as a deliberate
// abstention rather than a zero.
func QualityScoreLabel(s store.SessionSignals) string {
	if !s.Scored() {
		return "not scored"
	}
	return fmt.Sprintf("%d / 100", *s.Score)
}

// PeakContextLabel renders the heaviest single-turn context the session reached, in full
// tokens, for the Tokens tooltip. It is a window-independent measure of how loaded the
// session got. An unmeasured session (no usage) reads a dash rather than a misleading zero.
func PeakContextLabel(s store.SessionSignals) string {
	if s.PeakContextTokens == nil {
		return "-"
	}
	return FmtTokens(*s.PeakContextTokens)
}

// ContextResetsLabel renders how many inferred context resets (compactions or clears) the
// session went through, for the Tokens tooltip. An unmeasured session reads a dash; a
// measured session with none reads "0", a real "it never shed context".
func ContextResetsLabel(s store.SessionSignals) string {
	if s.ContextResetCount == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *s.ContextResetCount)
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

// FmtTokensCompact renders a token count to a short magnitude (2.1B, 1.7M, 63.0k,
// 412), for the feed's inline figure where the exact value lives in the hover
// card. The thousands-separated FmtTokens stays the form for places that show the
// full number. The buckets mirror fmtTok in static/charts.js, its client-side
// counterpart, so a figure reads the same on either surface.
func FmtTokensCompact(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FmtTime renders a timestamp in the viewer's timezone (UTC when none is set), or a
// dash when absent. It keeps the bare "2006-01-02 15:04" form for a visible cell;
// FmtTimeLong adds the zone abbreviation for a hover title, where naming the zone
// earns its width.
func FmtTime(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04")
}

// FmtTimeLong is FmtTime with the zone abbreviation appended, for the hover title
// on a stamp shown short elsewhere. Naming the zone (PST, UTC, ...) lets a reader
// tell which zone a full stamp is in without cluttering every visible cell with it.
func FmtTimeLong(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04 MST")
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
func FmtRelTime(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return relTime(time.Now(), *t, Loc(ctx))
}

// relTime is FmtRelTime's clock-injected core. Day distance is measured between
// calendar dates in the viewer's zone (not 24-hour windows), so a session from
// late last night reads "1 day ago" rather than "today" merely because fewer than
// 24 hours have passed, and the boundary is the reader's local midnight rather than
// UTC's. The absolute-stamp fallback also renders in the viewer's zone.
func relTime(now, t time.Time, loc *time.Location) string {
	now, t = now.In(loc), t.In(loc)
	nd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	td := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
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

// FmtTimeAt renders a non-pointer timestamp in the viewer's timezone, or a dash
// when zero.
func FmtTimeAt(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04")
}

// grantName renders a connected app's display name, falling back to a generic
// label when the client registered without one.
func grantName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Unnamed MCP client"
	}
	return name
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
