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
// to watch the status stream), so a finished rebuild brings the real page back.
func (s *Server) gatePublicParsed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.worker.FleetStatus(r.Context())
		if !st.InProgress {
			next(w, r)
			return
		}
		render(w, r, http.StatusOK, web.PublicReparsePage(st.Done, st.Total, st.Failed))
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
