// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

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
