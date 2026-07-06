// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"fmt"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

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
	return ProjectLabel(d.ProjectKind, d.ProjectName, d.ProjectKey)
}

// ProjectLabel is the folder-name-or-remote-key choice SessionProjectLabel makes, taking the
// three fields directly so the session OG card (which reads a store.SessionCard, not a full
// SessionDetail) resolves its heading through the same rule the page's <h1> uses.
func ProjectLabel(kind, name, key string) string {
	if IsLocalKind(kind) {
		return name
	}
	return key
}

// SessionPageTitle is the browser-tab title for a session view: the session's own
// summary when it has one (the same line the page's <h1> shows), else a stable
// "<project> session" label. Both the signed-in and the public session page use it,
// so a shared link and the in-app tab read the same rather than one saying "Session
// #42" and the other the project name.
func SessionPageTitle(d store.SessionDetail) string {
	if d.Title != "" {
		return d.Title
	}
	return SessionProjectLabel(d) + " session"
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

// SplitProjectFacets partitions the project filter options into git-remote repositories and
// local folders, preserving the store's busiest-first order within each group. The session
// toolbar renders the two as separate option groups so a reader scanning for a repository is
// not wading through a machine's scratch folders: a repository is the audit unit, a local
// folder the looser catch-all beneath it. The store already orders remotes ahead of locals
// (GlobalFacets), so this only routes each option to its bucket.
func SplitProjectFacets(projects []store.ProjectFacet) (repos, folders []store.ProjectFacet) {
	for _, pf := range projects {
		if IsLocalKind(pf.Kind) {
			folders = append(folders, pf)
		} else {
			repos = append(repos, pf)
		}
	}
	return repos, folders
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

// SubagentCollapseThreshold is the subagent count past which the session detail
// collapses the subagents table behind a summary line. Below it the short list reads
// fine inline; above it the table pushes the transcript below the fold on exactly the
// fan-out-heavy sessions a lead most wants to audit, so it folds away by default and the
// summary carries the count and spend at a glance. The value is a comfortable ceiling: a
// handful of subagents is context, a few dozen is a wall.
const SubagentCollapseThreshold = 8

// SubagentsCollapsed reports whether a session's subagents table should render collapsed
// behind its summary (more than SubagentCollapseThreshold children), so the template and a
// test read the same rule rather than each spelling out the comparison.
func SubagentsCollapsed(subs []store.SessionSummary) bool {
	return len(subs) > SubagentCollapseThreshold
}

// SubagentsSummaryLabel is the collapsed subagents summary: the direct child count and
// their summed cost ("34 subagents · $6.12"). It describes the subagents table it heads,
// which lists a session's direct children (Store.Subagents), so both figures are over those
// direct rows, not the feed fan-out chip's whole-subtree rollup (TreeRollup, which also
// folds in a subagent's own subagents). For a flat fan-out the two read the same; for a
// nested one they legitimately differ, because this summary answers "what is in the table
// below" while the feed chip answers "what did the whole work item cost". The cost carries
// the "+" lower-bound marker when any direct child could not be fully priced. The count is
// singular at one, though the collapse only ever engages well above that.
func SubagentsSummaryLabel(subs []store.SessionSummary) string {
	var cost float64
	var incomplete bool
	for _, s := range subs {
		cost += s.TotalCostUSD
		incomplete = incomplete || s.CostIncomplete
	}
	unit := "subagents"
	if len(subs) == 1 {
		unit = "subagent"
	}
	return fmt.Sprintf("%d %s · %s", len(subs), unit, FmtCost(cost, incomplete))
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

// grantName renders a connected app's display name, falling back to a generic
// label when the client registered without one.
func grantName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Unnamed MCP client"
	}
	return name
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

// DetailLabel renders a tool call's Detail (a command, pattern, URL, or other
// bounded input summary, up to 2048 bytes and possibly multi-line) as a single
// scannable line for a chip or outline step: every run of whitespace collapses to
// one space, and the result is capped at 80 runes with a trailing ellipsis. The
// cap keeps a chip from growing to the size of its input; the full text still
// reaches the reader through the element's title attribute, so nothing is lost,
// only deferred to hover. The truncation counts runes, not bytes, so it never
// splits a multi-byte UTF-8 sequence.
func DetailLabel(s string) string {
	const max = 80
	var b strings.Builder
	b.Grow(max + 4)
	space := false
	started := false
	emitted := 0
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if started {
				space = true
			}
			continue
		}
		if emitted >= max {
			b.WriteRune('…')
			return b.String()
		}
		if space {
			b.WriteByte(' ')
			emitted++
			space = false
			if emitted >= max {
				b.WriteRune('…')
				return b.String()
			}
		}
		b.WriteRune(r)
		emitted++
		started = true
	}
	return b.String()
}
