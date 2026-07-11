package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestBlobHEADUsesMetadataWithoutStreamingBody(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	body := []byte("Grace Hopper's tool output")
	sessionID, sha := seedToolSession(t, st, owner.ID, projectID, "head-blob", body)
	s := New(st, config.Server{}, parse.NewWorker(st, 1, 0))

	req := httptest.NewRequest(http.MethodHead, "/api/v1/session/blob", nil)
	recorder := httptest.NewRecorder()
	s.serveBlobForSession(recorder, req, sessionID, sha, "private, max-age=31536000, immutable")

	resp := recorder.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD blob = %d, want 200", resp.StatusCode)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Fatalf("HEAD handler streamed %d body bytes, want none", got)
	}
	if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
}
