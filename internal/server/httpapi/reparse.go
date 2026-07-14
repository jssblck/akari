package httpapi

import (
	"encoding/json"
	"net/http"
)

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
			"code":    "projection_rebuild",
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
