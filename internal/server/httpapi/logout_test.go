package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestLogoutSurfacesSessionDeletionFailure(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	s := New(st, config.Server{}, parse.NewWorker(st, 1, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "session-secret"})
	recorder := httptest.NewRecorder()

	s.handleLogout(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("logout = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	setCookie := recorder.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, cookieName+"=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("logout did not clear the browser cookie after store failure: %q", setCookie)
	}
}
