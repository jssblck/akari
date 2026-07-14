// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
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
// "<project> session" label. The public session shell and its OG card both use it,
// so a shared link and the in-app tab read the same rather than one saying "Session
// #42" and the other the project name.
func SessionPageTitle(d store.SessionDetail) string {
	if d.Title != "" {
		return d.Title
	}
	return SessionProjectLabel(d) + " session"
}
