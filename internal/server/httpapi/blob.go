package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/server/store"
)

// handleSessionBlob streams a CAS blob to an authenticated viewer. Access is
// gated on the session referencing the hash, not on the bare hash, so the
// content-addressed dedup cannot leak a body through a session that does not use
// it.
func (s *Server) handleSessionBlob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Any authenticated full-scope viewer may see internal sessions, so existence
	// plus the reference check is sufficient here.
	if _, err := s.Store.SessionDetailByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveBlobForSession(w, r, id, r.PathValue("sha256"))
}

// handlePublicBlob streams a CAS blob to a logged-out viewer, reached only
// through a published session's public id and only for a hash that session
// references.
func (s *Server) handlePublicBlob(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.SessionDetailByPublicID(r.Context(), r.PathValue("public_id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveBlobForSession(w, r, d.ID, r.PathValue("sha256"))
}

// serveBlobForSession verifies the session references the hash, then streams the
// blob's stored bytes. The Content-Type is the body's semantic media type so a
// reader knows how to render it; when the stored bytes are zstd-compressed the
// Content-Encoding tells the client (browser or API consumer) to decompress
// transparently, which it does without the server ever touching the bytes. nosniff
// keeps a browser from reinterpreting a stored body as a richer type than it is.
func (s *Server) serveBlobForSession(w http.ResponseWriter, r *http.Request, sessionID int64, sha string) {
	sha = strings.ToLower(sha)
	if !isHexSHA256(sha) {
		http.NotFound(w, r)
		return
	}
	ok, err := s.Store.SessionReferencesBlob(r.Context(), sessionID, sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "look up blob")
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	meta, err := s.Store.BlobMeta(r.Context(), sha)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, "load blob")
		return
	}
	w.Header().Set("Content-Type", safeBlobContentType(meta.MediaType))
	if meta.ContentType == parser.ContentZstd {
		// The stored bytes are zstd; advertise it so the client decompresses
		// transparently. The server streams the compressed bytes untouched.
		w.Header().Set("Content-Encoding", "zstd")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if _, err := s.Store.WriteBlobTo(r.Context(), w, sha); err != nil {
		// Headers are already committed; nothing left but to drop the connection.
		return
	}
}

// safeBlobContentType maps a stored media type to one safe to serve inline. The
// CAS only ever stores JSON, plain text, or opaque bytes; anything unexpected is
// served as opaque bytes so it can never be interpreted as active content.
func safeBlobContentType(mediaType string) string {
	switch mediaType {
	case "application/json":
		return "application/json; charset=utf-8"
	case "text/plain", "":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// isHexSHA256 reports whether s is a 64-character lowercase hex string.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
