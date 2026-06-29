package httpapi

import (
	"errors"
	"net/http"
	"strings"

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
// against the sweep until the transcript that references it lands. The body
// streams in from the request so neither side buffers it whole. The server
// verifies the streamed bytes hash to the path's sha256, so a corrupt or
// mislabeled upload is rejected rather than poisoning the content-addressed store.
func (s *Server) handleBlobUpload(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha256"))
	if !isHexSHA256(sha) {
		writeError(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	mediaType := strings.TrimSpace(r.URL.Query().Get("media_type"))

	body := http.MaxBytesReader(w, r.Body, maxBlobUpload)
	defer body.Close()
	if err := s.Store.PutBlob(r.Context(), sha, mediaType, body); err != nil {
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
