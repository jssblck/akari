package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jssblck/akari/internal/server/store"
)

// handleAPISessionAppend backs the session page's live refresh: after an SSE
// "update" wake, the client asks for only the turns past the last ordinal it has
// rendered rather than refetching the whole snapshot, so a long-running session's
// live tail never blows away transcript pages the reader already scrolled past.
// It mirrors handleAPISession's response shape (the same SessionSnapshot under
// "snapshot") so the client merges an append the same way it read the initial
// load, and returns an empty Page when the tick was quiet (raw bytes ahead of the
// rebuild changed no turns; see store.SessionAppendByID).
func (s *Server) handleAPISessionAppend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "session")
	if !ok {
		return
	}
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || after < 0 {
		writeError(w, http.StatusBadRequest, "invalid after cursor")
		return
	}
	snapshot, err := s.Store.SessionAppendByID(r.Context(), id, after)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session update")
		return
	}
	p, _ := principalFrom(r.Context())
	viewer, _ := s.Store.UserByID(r.Context(), p.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snapshot, "owner": snapshot.Audit.Detail.OwnerID == p.UserID,
		"can_delete": snapshot.Audit.Detail.OwnerID == p.UserID || viewer.IsAdmin,
	})
}
