package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

// maxChunk bounds a single uploaded chunk. The client streams a file as several
// message-aligned chunks, but one oversized message (a JSONL line, or a folded
// Codex turn) is served alone, so this matches the client's hard cap. The server
// parses one chunk in one region, so this also bounds worst-case parse memory.
const maxChunk = 128 << 20

var validAgents = map[string]bool{"claude": true, "codex": true, "pi": true}

var validKinds = map[string]bool{"remote": true, "standalone": true, "orphaned": true}

type announceRequest struct {
	Agent           string `json:"agent"`
	SourceSessionID string `json:"source_session_id"`
	Kind            string `json:"kind"`
	ProjectRemote   string `json:"project_remote"`
	LocalRoot       string `json:"local_root"`
	GitBranch       string `json:"git_branch"`
	Cwd             string `json:"cwd"`
	Machine         string `json:"machine"`
	// Terminal is set by `akari sync --finalize`: the client asserts this session is
	// finished, so the server can grade it now instead of waiting out the idle window.
	// Absent (older clients, an ordinary sync) it decodes false.
	Terminal bool `json:"terminal"`
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
	req.LocalRoot = strings.TrimSpace(req.LocalRoot)
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Kind == "" {
		req.Kind = "remote" // back-compat: older clients announce only remotes
	}
	if !validAgents[req.Agent] {
		writeError(w, http.StatusBadRequest, "agent must be claude, codex, or pi")
		return
	}
	if req.SourceSessionID == "" {
		writeError(w, http.StatusBadRequest, "source_session_id is required")
		return
	}
	if !validKinds[req.Kind] {
		writeError(w, http.StatusBadRequest, "kind must be remote, standalone, or orphaned")
		return
	}

	// A remote session carries a canonical remote key; a standalone or orphaned
	// session has none, so the server derives a stable per-machine, per-path key.
	var remoteKey, host, owner, repo, displayName string
	if req.Kind == "remote" {
		var ok bool
		host, owner, repo, ok = parseRemoteKey(req.ProjectRemote)
		if !ok {
			writeError(w, http.StatusBadRequest, "project_remote must look like host/owner/repo")
			return
		}
		remoteKey, displayName = req.ProjectRemote, repo
	} else {
		remoteKey, displayName = localProjectIdentity(req.Machine, req.Cwd, req.LocalRoot)
		host = req.Machine
		repo = displayName
	}

	res, err := s.Store.AnnounceWithProject(r.Context(), store.AnnounceParams{
		UserID:          p.UserID,
		Agent:           req.Agent,
		SourceSessionID: req.SourceSessionID,
		Kind:            req.Kind,
		GitBranch:       req.GitBranch,
		Cwd:             req.Cwd,
		Machine:         req.Machine,
		Terminal:        req.Terminal,
	}, store.ProjectParams{
		RemoteKey:   remoteKey,
		Host:        host,
		Owner:       owner,
		Repo:        repo,
		DisplayName: displayName,
		Kind:        req.Kind,
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
	sessionID, _, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset query parameter is required")
		return
	}
	// Refresh the read deadline as the chunk arrives so a large (up to 128 MiB)
	// chunk on a slow link is not cut off by the server-wide ReadTimeout, while a
	// stalled client still trips the idle deadline (see deadlines.go).
	data, err := io.ReadAll(idleReadDeadline(w, http.MaxBytesReader(w, r.Body, maxChunk)))
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
	case errors.Is(err, store.ErrChunkNotLineAligned):
		writeError(w, http.StatusBadRequest, "chunk must end on a newline")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "append chunk")
		return
	}
	// The raw bytes are committed; parsing is the worker's job. The append itself
	// marked the session due (byte_len moved past the last rebuilt length), so the
	// wake buys latency, not correctness, and client ingest health never depends
	// on parser correctness. The SSE publish to watching browsers happens when the
	// rebuild commits (the worker's rebuilt hook), when there is actually a new
	// projection to fetch.
	s.worker.Wake()
	writeJSON(w, http.StatusOK, map[string]any{"stored_bytes": stored})
}

// handleFinalize grades a terminal session immediately, rather than leaving it for
// the next settle tick. The client calls it at the end of an `akari sync --finalize`
// once the whole transcript has landed: the session was announced terminal, so its
// signals now derive with the idle checks satisfied. On an ephemeral host (CI, a
// cloud sandbox) this lands the grade before the host disappears, instead of waiting
// out the 30-minute abandoned-idle window (and a settle loop that may be disabled).
//
// It is idempotent and safe to call on any owned session: a session that is not
// terminal simply grades under the ordinary rules (unknown until it settles), and a
// re-call recomputes the same row. The grade is derived from the projection already
// committed by the preceding chunks, so no request body is read.
func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	sessionID, _, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	if err := s.Store.RefreshSessionSignals(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "finalize session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"finalized": true})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	sessionID, _, ok := s.ownedSession(w, r)
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
// principal owns that session, returning the session id and its agent. It writes
// the error response itself on failure.
func (s *Server) ownedSession(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return 0, "", false
	}
	owner, agent, err := s.Store.SessionMeta(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return 0, "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "look up session")
		return 0, "", false
	}
	if owner != p.UserID {
		writeError(w, http.StatusForbidden, "session belongs to another user")
		return 0, "", false
	}
	return id, agent, true
}

// localProjectIdentity derives the project key and display name for a session
// with no git remote. A standalone session in a live git worktree reports its
// localRoot (the main worktree shared by every worktree of the repo), so all of
// them collapse onto one key and display as the repo folder, the way a canonical
// remote collapses a remote-backed repo's worktrees. Without a root (a non-git
// folder, an orphaned session whose worktree is gone, or an older client) it
// falls back to the per-session cwd, so the folder still groups by its own
// location. A worktree that is later archived loses its root and so pops out into
// its own location-keyed project: the live repo group is unaffected, and the
// archived case is the one with no reliable parent signal anyway.
func localProjectIdentity(machine, cwd, localRoot string) (key, displayName string) {
	root := localRoot
	if root == "" {
		root = cwd
	}
	displayName = lastPathSegment(root)
	if displayName == "" {
		displayName = "(unknown location)"
	}
	return localProjectKey(machine, root), displayName
}

// localProjectKey derives the project key for a session with no git remote. It
// groups by machine and a local location (a repo root for a live worktree, else
// the working directory), so every standalone or orphaned session that shares
// that location on the same machine lands in one project. The "local:" prefix and
// the colon separators keep it out of the "host/owner/repo" remote namespace: a
// canonicalized remote key has no empty path segments and is never shaped like
// this, so a synthetic key can never collide with a real one. Standalone and
// orphaned share the namespace (the key omits the kind) so a folder that is
// deleted transitions kind in place rather than forking.
func localProjectKey(machine, location string) string {
	return "local:" + machine + ":" + location
}

// lastPathSegment returns the final element of a filesystem path, accepting both
// forward and back slashes so a Windows client's path renders sensibly on the
// Linux server. It returns "" for an empty path.
func lastPathSegment(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
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
