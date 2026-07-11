package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestBlobUploadLimitIsConsistentForNewAndDuplicateHashes(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	s := New(st, config.Server{}, parse.NewWorker(st, 1, 0))
	const limit = int64(8)

	existingBody := "existing"
	existingSHA := store.HashString(existingBody)
	if err := st.PutBlob(ctx, existingSHA, "text/plain", parser.ContentRaw, strings.NewReader(existingBody)); err != nil {
		t.Fatalf("seed existing blob: %v", err)
	}

	upload := func(sha, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/ingest/blob/"+sha, strings.NewReader(body))
		// Exercise the streaming limit. A known Content-Length would be rejected
		// before the CAS's duplicate-body drain runs.
		req.ContentLength = -1
		recorder := httptest.NewRecorder()
		s.storeBlobUpload(recorder, req, sha, "text/plain", parser.ContentRaw, limit)
		return recorder
	}

	if got := upload(existingSHA, "too-large").Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized duplicate upload = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}

	newBody := "new-large"
	newSHA := store.HashString(newBody)
	if got := upload(newSHA, newBody).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized new upload = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
	if _, err := st.BlobMeta(ctx, newSHA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oversized new upload left a blob behind: %v", err)
	}
}
