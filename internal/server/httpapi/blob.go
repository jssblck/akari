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
	s.serveBlobForSession(w, r, id, r.PathValue("sha256"), "private, max-age=31536000, immutable")
}

// handlePublicBlob streams a CAS blob to a logged-out viewer, reached only
// through a published session's public id and only for a hash that session
// references.
func (s *Server) handlePublicBlob(w http.ResponseWriter, r *http.Request) {
	// The public-id capability can be revoked. Do not let a browser satisfy a
	// later request from its immutable cache after the owner makes the session
	// private; every access must re-check publication and blob reference state.
	w.Header().Set("Cache-Control", "no-store")
	d, err := s.Store.SessionDetailByPublicID(r.Context(), r.PathValue("public_id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveBlobForSession(w, r, d.ID, r.PathValue("sha256"), "no-store")
}

// serveBlobForSession verifies the session references the hash, then streams the
// blob's stored bytes. The Content-Type is the body's semantic media type so a
// reader knows how to render it; when the stored bytes are zstd-compressed the
// Content-Encoding tells the client (browser or API consumer) to decompress
// transparently, which it does without the server ever touching the bytes. nosniff
// keeps a browser from reinterpreting a stored body as a richer type than it is.
func (s *Server) serveBlobForSession(w http.ResponseWriter, r *http.Request, sessionID int64, sha, cacheControl string) {
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
	w.Header().Set("Content-Length", strconv.FormatInt(meta.ByteLen, 10))
	// The URL is content-addressed by sha256, so the bytes behind it can never
	// change: the sha is a free, perfect strong validator. Authenticated routes
	// serve it immutable with a far-future max-age, while the revocable public
	// route preselects no-store above. The ETag still lets an explicit revalidation
	// avoid retransferring a large lifted image after access is checked. The
	// authenticated cache stays private because gating is per-session: a shared
	// cache must not serve one session's body to a viewer who never referenced it.
	etag := `"` + sha + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheControl)
	if ifNoneMatchHas(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Refresh the write deadline as the body streams so a large blob to a slow
	// client is not truncated by the server-wide WriteTimeout; see deadlines.go.
	if _, err := s.Store.WriteBlobTo(r.Context(), idleWriteDeadline(w), sha); err != nil {
		// Headers are already committed; nothing left but to drop the connection.
		return
	}
}

// ifNoneMatchHas reports whether an If-None-Match header names the given quoted
// ETag. The header is a comma-separated list and may be "*", so it is parsed as a
// list rather than compared whole. RFC 7232 requires the weak comparison function
// for If-None-Match, so a W/ prefix on either side is stripped before comparing
// opaque-tags: a client that revalidates with W/"<sha>" still gets a 304 against
// our strong "<sha>". Weak comparison is exactly right here anyway, since the bytes
// behind a content-addressed URL never change.
func ifNoneMatchHas(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}

// safeBlobContentType maps a stored media type to one safe to serve inline. The CAS
// stores JSON and plain text (tool bodies) and raster images (lifted attachments); a
// raster image is served under its real type so the transcript's <img> renders it,
// while anything else (notably image/svg+xml, which can carry script) is served as
// opaque bytes so it can never be interpreted as active content. With nosniff set, the
// browser honors exactly the type named here and never up-sniffs octet-stream.
func safeBlobContentType(mediaType string) string {
	switch mediaType {
	case "application/json":
		return "application/json; charset=utf-8"
	case "text/plain", "":
		return "text/plain; charset=utf-8"
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		// The inert raster types the image sniffer emits: safe to render inline, and the
		// only image media a lifted attachment can carry (SVG is never sniffed, so a
		// data-URI claiming it falls through to octet-stream below).
		return mediaType
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
