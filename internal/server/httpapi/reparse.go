package httpapi

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jssblck/akari/internal/server/web"
)

// gateParsed wraps a server-rendered page that shows parsed/projected session data.
// While a fleet rebuild drains the corpus session by session (an epoch rollout, or an
// operator-triggered reparse), a cross-session view can mix already-rebuilt and
// not-yet-rebuilt sessions, so instead of the normal page it renders a "rebuild in
// progress" view with a live progress bar. An htmx partial swap gets just the banner
// fragment so an in-page list swap shows the same state. The gate is best-effort:
// each session is rebuilt atomically (no empty or half-built rows), so a request that
// races a rebuild starting mid-render only ever sees a mix of valid sessions. It runs
// inside requireReadHTML, so the principal is already on the request.
func (s *Server) gateParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.worker.FleetStatus(r.Context())
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
// to watch the status stream), so a finished rebuild brings the real page back. Like
// gateParsed, an HX-Request gets the small banner fragment instead of the full page:
// the public transcript's "Show earlier" button fetches /s/{public_id}/body with
// hx-swap="outerHTML", so a full document there would swap an entire page into the
// button's DOM slot rather than replacing the button.
func (s *Server) gatePublicParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// These URLs are capabilities the owner can revoke at any moment. Apply the
		// policy before the fleet gate so published pages, revoked-link 404s, backend
		// errors, and the temporary reparse stand-in can never be replayed from a
		// browser or shared cache after publication state changes.
		w.Header().Set("Cache-Control", "no-store")
		st := s.worker.FleetStatus(r.Context())
		if !st.InProgress {
			next(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			render(w, r, http.StatusOK, web.ReparseBanner(st.Done, st.Total, st.Failed))
			return
		}
		render(w, r, http.StatusOK, web.PublicReparsePage(st.Done, st.Total, st.Failed))
	}
}

// gateAPIParsed prevents a cross-session JSON read from mixing projection
// generations while a fleet rebuild is draining. A single session rebuild is
// atomic, but aggregate and feed responses can otherwise combine sessions from
// opposite sides of the epoch boundary.
func (s *Server) gateAPIParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := s.worker.FleetStatus(r.Context())
		if !status.InProgress {
			next(w, r)
			return
		}
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "projection rebuild in progress",
			"reparse": status,
		})
	}
}

// handleReparseStatus returns the current fleet-rebuild status as JSON, the
// poll-fallback source for the progress UI when the SSE stream is unavailable. It
// reports fleet status, so an instance that is only observing another's rebuild
// still tells its pages to stay gated.
func (s *Server) handleReparseStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.worker.FleetStatus(r.Context()))
}

// handleReparseForm is the admin Reparse button: it marks the whole corpus due (a
// fleet rebuild regardless of the epoch) and redirects back to the account page,
// where the live status takes over. Marking an already-due corpus again is
// harmless, so a double click cannot corrupt anything; the worker drains it once.
func (s *Server) handleReparseForm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.worker.Trigger(r.Context(), ""); err != nil {
		log.Printf("reparse trigger: %v", err)
		writeError(w, http.StatusInternalServerError, "trigger reparse")
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleReparseEvents streams fleet-rebuild progress to a watching browser over
// SSE, pushing the status JSON as each frame so the page updates its progress bar
// directly.
func (s *Server) handleReparseEvents(w http.ResponseWriter, r *http.Request) {
	ch := s.hub.subscribeReparse()
	defer s.hub.unsubscribeReparse(ch)
	statusFrame := func(payload string) string { return "event: status\ndata: " + payload + "\n\n" }
	serveSSE(w, r, ch, statusFrame, func(write func(string) bool) bool {
		// Paint the current status immediately so a page that connects mid-rebuild
		// (or after it finished) does not wait for the next frame to learn the state.
		b, err := json.Marshal(s.worker.FleetStatus(r.Context()))
		if err != nil {
			return true
		}
		return write(statusFrame(string(b)))
	})
}
