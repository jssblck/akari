package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jssblck/akari/internal/server/reparse"
	"github.com/jssblck/akari/internal/server/web"
)

// gateParsed wraps a server-rendered page that shows parsed/projected session data.
// While a reparse is rebuilding the projection in place, those rows are stale or
// half-rebuilt, so instead of the normal page it renders a "reparse in progress"
// view with a live progress bar. An htmx partial swap gets just the banner fragment
// so an in-page list swap shows the same state. It runs inside requireReadHTML, so
// the principal is already on the request for the page shell.
func (s *Server) gateParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.reparser.FleetStatus(r.Context())
		if !st.InProgress {
			next(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			render(w, r, http.StatusOK, web.ReparseBanner(st.Done, st.Total, st.Failed))
			return
		}
		render(w, r, http.StatusOK, web.ReparseProgressPage(s.pageForNav(r, "Reparsing", ""), st.Done, st.Total, st.Failed))
	}
}

// gatePublicParsed is gateParsed for the logged-out public session view, which has
// no app shell. The public progress page reloads on a timer (it has no credential
// to watch the status stream), so a finished reparse brings the real page back.
func (s *Server) gatePublicParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.reparser.FleetStatus(r.Context())
		if !st.InProgress {
			next(w, r)
			return
		}
		render(w, r, http.StatusOK, web.PublicReparsePage(st.Done, st.Total, st.Failed))
	}
}

// handleReparseStatus returns the current reparse status as JSON, the poll-fallback
// source for the progress UI when the SSE stream is unavailable. It reports fleet
// status, so a follower instance that is only observing another's reparse still
// tells its pages to stay gated.
func (s *Server) handleReparseStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reparser.FleetStatus(r.Context()))
}

// handleReparseForm is the admin Reparse button: it forces a reparse (regardless of
// the epoch) and redirects back to the account page, where the live status takes
// over. Trigger is a no-op when one is already running, so a double click cannot
// start two.
func (s *Server) handleReparseForm(w http.ResponseWriter, r *http.Request) {
	s.reparser.Trigger(reparse.Options{Force: true})
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleReparseEvents streams reparse progress to a watching browser over SSE,
// pushing the status JSON as each frame so the page updates its progress bar
// directly. It mirrors handleSessionEvents: a bounded per-write deadline turns a
// stalled client into a write error so the subscription never leaks, and a periodic
// comment keeps the connection alive through idle proxies.
func (s *Server) handleReparseEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	write := func(payload string) bool {
		if rc.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprint(w, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	ch := s.hub.subscribeReparse()
	defer s.hub.unsubscribeReparse(ch)

	if !write(": connected\n\n") {
		return
	}
	// Paint the current status immediately so a page that connects mid-reparse (or
	// after it finished) does not wait for the next frame to learn the state.
	if b, err := json.Marshal(s.reparser.FleetStatus(r.Context())); err == nil {
		if !write("event: status\ndata: " + string(b) + "\n\n") {
			return
		}
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-ch:
			if !write("event: status\ndata: " + payload + "\n\n") {
				return
			}
		case <-keepalive.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
