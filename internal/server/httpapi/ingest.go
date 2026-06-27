package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

// maxChunk bounds a single uploaded chunk. The client streams larger files as
// several newline-terminated chunks.
const maxChunk = 64 << 20

var validAgents = map[string]bool{"claude": true, "codex": true, "pi": true}

type announceRequest struct {
	Agent           string `json:"agent"`
	SourceSessionID string `json:"source_session_id"`
	ProjectRemote   string `json:"project_remote"`
	GitBranch       string `json:"git_branch"`
	Cwd             string `json:"cwd"`
	Machine         string `json:"machine"`
}

func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req announceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.SourceSessionID = strings.TrimSpace(req.SourceSessionID)
	req.ProjectRemote = strings.TrimSpace(req.ProjectRemote)
	if !validAgents[req.Agent] {
		writeError(w, http.StatusBadRequest, "agent must be claude, codex, or pi")
		return
	}
	if req.SourceSessionID == "" {
		writeError(w, http.StatusBadRequest, "source_session_id is required")
		return
	}
	host, owner, repo, ok := parseRemoteKey(req.ProjectRemote)
	if !ok {
		writeError(w, http.StatusBadRequest, "project_remote must look like host/owner/repo")
		return
	}
	projectID, err := s.Store.UpsertProject(r.Context(), req.ProjectRemote, host, owner, repo, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert project")
		return
	}
	res, err := s.Store.Announce(r.Context(), store.AnnounceParams{
		UserID:          p.UserID,
		Agent:           req.Agent,
		SourceSessionID: req.SourceSessionID,
		ProjectID:       projectID,
		GitBranch:       req.GitBranch,
		Cwd:             req.Cwd,
		Machine:         req.Machine,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "announce session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    res.SessionID,
		"stored_bytes":  res.StoredBytes,
		"prefix_sha256": res.PrefixSHA256,
	})
}

func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset query parameter is required")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunk))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "chunk too large or read error")
		return
	}
	stored, err := s.Store.AppendChunk(r.Context(), sessionID, offset, data)
	var mismatch store.OffsetMismatchError
	switch {
	case errors.As(err, &mismatch):
		// Idempotent resync: tell the client the true cursor so it can advance.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "offset mismatch", "stored_bytes": mismatch.StoredBytes,
		})
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "append chunk")
		return
	}
	// message_count is 0 until the parser lands (milestone 2).
	writeJSON(w, http.StatusOK, map[string]any{"stored_bytes": stored, "message_count": 0})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	if err := s.Store.ResetRaw(r.Context(), sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "reset session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored_bytes": 0})
}

// ownedSession parses the {id} path value and confirms the authenticated
// principal owns that session. It writes the error response itself on failure.
func (s *Server) ownedSession(w http.ResponseWriter, r *http.Request) (int64, bool) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return 0, false
	}
	owner, err := s.Store.SessionOwner(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return 0, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "look up session")
		return 0, false
	}
	if owner != p.UserID {
		writeError(w, http.StatusForbidden, "session belongs to another user")
		return 0, false
	}
	return id, true
}

// parseRemoteKey splits a canonical "host/owner/.../repo" key into host, owner,
// and repo. The owner is everything between host and the final repo segment,
// which keeps nested groups (for example GitLab subgroups) intact.
func parseRemoteKey(key string) (host, owner, repo string, ok bool) {
	segs := strings.Split(key, "/")
	if len(segs) < 3 {
		return "", "", "", false
	}
	for _, s := range segs {
		if s == "" {
			return "", "", "", false
		}
	}
	host = segs[0]
	repo = segs[len(segs)-1]
	owner = strings.Join(segs[1:len(segs)-1], "/")
	return host, owner, repo, true
}
