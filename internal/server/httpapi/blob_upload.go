package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/server/store"
)

// maxBlobUpload bounds a single tool body upload. A body the transcript no longer
// inlines can be very large (a 508 MiB turn of base64 image results is the
// motivating case), so the cap is generous; the body still streams through to the
// large object in bounded slices, so a large upload never costs a large server
// buffer. The cap only refuses a body so pathological it would not be worth
// storing at all.
const maxBlobUpload = 2 << 30 // 2 GiB

type blobCheckRequest struct {
	SHA256 []string `json:"sha256"`
}

// handleBlobCheck reports which of a set of candidate tool-body hashes the CAS
// lacks, so the client uploads only those. It also pins every hash it finds
// present: the client skips the PUT for a present body, so the body must be held
// against the sweep until the transcript chunk that references it commits. Without
// the pin a present, unreferenced body could be reclaimed between the check and the
// transcript append, stranding a sentinel with no body. The CAS dedupes globally,
// so a body any session already stored is reported absent from the missing set and
// not re-sent.
func (s *Server) handleBlobCheck(w http.ResponseWriter, r *http.Request) {
	var req blobCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	clean := make([]string, 0, len(req.SHA256))
	for _, sha := range req.SHA256 {
		sha = strings.ToLower(strings.TrimSpace(sha))
		if isHexSHA256(sha) {
			clean = append(clean, sha)
		}
	}
	missing, err := s.Store.MissingBlobs(r.Context(), clean)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check blobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

// handleBlobUpload stores one tool body in the CAS under its sha256 and pins it
// against the sweep until the transcript that references it lands. The body streams
// in from the request as the STORED bytes (raw or zstd, per the content_type query
// param) so neither side buffers it whole and the server never compresses. The
// server verifies the streamed bytes hash to the path's sha256, so a corrupt or
// mislabeled upload is rejected rather than poisoning the content-addressed store.
func (s *Server) handleBlobUpload(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha256"))
	if !isHexSHA256(sha) {
		writeError(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	mediaType := strings.TrimSpace(r.URL.Query().Get("media_type"))
	contentType, ok := normalizeStorageContentType(r.URL.Query().Get("content_type"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid blob content_type")
		return
	}

	s.storeBlobUpload(w, r, sha, mediaType, contentType, maxBlobUpload)
}

// storeBlobUpload applies the byte limit while streaming the request into the
// CAS. The limit is an argument so the over-limit behavior can be exercised with
// a small body instead of allocating a multi-gigabyte test fixture.
func (s *Server) storeBlobUpload(w http.ResponseWriter, r *http.Request, sha, mediaType, contentType string, limit int64) {
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds upload limit")
		return
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	tracked := &blobLimitReader{r: idleReadDeadline(w, body)}
	// A 2 GiB blob can take longer than the server-wide ReadTimeout to receive on
	// a slow link; refresh the read deadline as bytes arrive so an actively
	// progressing upload is not aborted mid-stream (see deadlines.go).
	err := s.Store.PutBlob(r.Context(), sha, mediaType, contentType, tracked)
	if tracked.exceeded {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds upload limit")
		return
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds upload limit")
			return
		}
		// A hash mismatch is the client's error (the bytes do not match the declared
		// key); everything else is a server fault.
		if errors.Is(err, store.ErrBlobHashMismatch) {
			writeError(w, http.StatusBadRequest, "uploaded bytes do not match declared hash")
			return
		}
		writeError(w, http.StatusInternalServerError, "store blob")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha256": sha})
}

// blobLimitReader records MaxBytesReader's sentinel even when the CAS duplicate
// path deliberately ignores a drain error. The handler can therefore return 413
// consistently for both new and already-present hashes.
type blobLimitReader struct {
	r        io.Reader
	exceeded bool
}

func (r *blobLimitReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		r.exceeded = true
	}
	return n, err
}

// normalizeStorageContentType validates the declared storage encoding of an upload.
// The server stores and serves these bytes opaquely, so it accepts only the two
// encodings it knows how to label on the way back out (raw or zstd) and rejects
// anything else fail-closed rather than storing a body it could not serve correctly.
// An absent value defaults to raw, the encoding of a small uncompressed body.
func normalizeStorageContentType(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", parser.ContentRaw:
		return parser.ContentRaw, true
	case parser.ContentZstd:
		return parser.ContentZstd, true
	default:
		return "", false
	}
}
