package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func newPasswordTestServer(t *testing.T, st *store.Store, work *passwordWork) *httptest.Server {
	t.Helper()
	server := New(st, config.Server{}, parse.NewWorker(st, 1, 0))
	server.passwords = work
	server.authAttempts = &authAttemptLimiter{
		accounts: newAttemptBuckets(60_000, 1_000, 100),
		sources:  newAttemptBuckets(60_000, 1_000, 100),
	}
	srv := httptest.NewServer(server.Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestPasswordAuthRequestConcurrency(t *testing.T) {
	st := storetest.NewStore(t)
	if _, err := st.Register(context.Background(), "grace", "stored-hash", ""); err != nil {
		t.Fatalf("register local account: %v", err)
	}
	if _, err := st.UpsertProxyUser(context.Background(), "ada"); err != nil {
		t.Fatalf("register federated account: %v", err)
	}
	t.Run("failure path normalization", func(t *testing.T) { testLoginFailuresShareVerificationAndResponsePath(t, st) })
	t.Run("normal parallel logins", func(t *testing.T) { testNormalParallelLoginsCompleteWithinWorkBudget(t, st) })
	t.Run("login flood", func(t *testing.T) { testLoginFloodRejectsBeyondBoundedQueue(t, st) })
	t.Run("registration flood", func(t *testing.T) { testRegistrationFloodSharesPasswordWorkBudget(t, st) })
	t.Run("verifier error", func(t *testing.T) { testPasswordWorkErrorsRemainStableLoginFailures(t, st) })
}

func testLoginFailuresShareVerificationAndResponsePath(t *testing.T, st *store.Store) {
	var mu sync.Mutex
	var hashes []string
	work := newPasswordWorkWithOperations(2, 4, time.Second, passwordOperations{
		verify: func(_ string, encoded string) (bool, error) {
			mu.Lock()
			hashes = append(hashes, encoded)
			mu.Unlock()
			return false, nil
		},
	})
	work.dummyHash = "dummy-hash"
	srv := newPasswordTestServer(t, st, work)

	for _, username := range []string{"grace", "ada", "unknown"} {
		status, body := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/login", `{"username":"`+username+`","password":"wrong"}`)
		if status != http.StatusUnauthorized || body["error"] != "invalid credentials" {
			t.Fatalf("login %q: status=%d body=%v", username, status, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hashes) != 3 || hashes[0] != "stored-hash" || hashes[1] != "dummy-hash" || hashes[2] != "dummy-hash" {
		t.Fatalf("verification hashes = %v, want real then two dummy hashes", hashes)
	}
}

func testNormalParallelLoginsCompleteWithinWorkBudget(t *testing.T, st *store.Store) {
	const requests = 12
	started := make(chan struct{}, requests)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	work := newPasswordWorkWithOperations(3, requests, time.Second, passwordOperations{
		verify: func(password, encoded string) (bool, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := peak.Load()
				if current <= old || peak.CompareAndSwap(old, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			return password == "correct" && encoded == "stored-hash", nil
		},
	})
	work.dummyHash = "dummy-hash"
	srv := newPasswordTestServer(t, st, work)

	statuses := make(chan int, requests)
	for range requests {
		go func() {
			status, _ := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"correct"}`)
			statuses <- status
		}()
	}
	for range 3 {
		<-started
	}
	close(release)
	for range requests {
		if status := <-statuses; status != http.StatusOK {
			t.Errorf("parallel login status = %d, want 200", status)
		}
	}
	if got := peak.Load(); got != 3 {
		t.Fatalf("peak verification work = %d, want 3", got)
	}
}

func testLoginFloodRejectsBeyondBoundedQueue(t *testing.T, st *store.Store) {
	const requests = 10
	started := make(chan struct{}, requests)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	var calls atomic.Int32
	work := newPasswordWorkWithOperations(1, 2, time.Second, passwordOperations{
		verify: func(string, string) (bool, error) {
			calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			if current > peak.Load() {
				peak.Store(current)
			}
			started <- struct{}{}
			<-release
			return false, nil
		},
	})
	work.dummyHash = "dummy-hash"
	srv := newPasswordTestServer(t, st, work)

	statuses := make(chan int, requests)
	for range requests {
		go func() {
			status, _ := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"wrong"}`)
			statuses <- status
		}()
	}
	<-started
	unauthorized := 0
	for range requests - 3 {
		if status := <-statuses; status == http.StatusUnauthorized {
			unauthorized++
		} else {
			t.Errorf("flood login status = %d, want 401", status)
		}
	}
	close(release)
	for range 3 {
		if status := <-statuses; status == http.StatusUnauthorized {
			unauthorized++
		} else {
			t.Errorf("admitted flood login status = %d, want 401", status)
		}
	}
	if unauthorized != requests {
		t.Fatalf("unauthorized responses = %d, want %d", unauthorized, requests)
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("peak verification work = %d, want at most 1", got)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("verification calls = %d, want exactly 3 admitted calls", got)
	}
}

func testRegistrationFloodSharesPasswordWorkBudget(t *testing.T, st *store.Store) {
	const requests = 10
	started := make(chan struct{}, requests)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	var calls atomic.Int32
	work := newPasswordWorkWithOperations(1, 2, time.Second, passwordOperations{
		hash: func(string) (string, error) {
			calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			if current > peak.Load() {
				peak.Store(current)
			}
			started <- struct{}{}
			<-release
			return "stored-hash", nil
		},
	})
	work.dummyHash = "dummy-hash"
	srv := newPasswordTestServer(t, st, work)
	admin, err := st.UserByUsername(context.Background(), "grace")
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}
	invite, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new invite: %v", err)
	}
	if _, err := st.CreateInvite(context.Background(), auth.HashToken(invite), admin.ID, "flood test", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	statuses := make(chan int, requests)
	for i := range requests {
		go func() {
			body, err := json.Marshal(map[string]string{
				"username": "ada-" + time.Unix(int64(i), 0).UTC().Format("150405"),
				"password": "password", "invite_token": invite,
			})
			if err != nil {
				t.Errorf("marshal registration: %v", err)
				return
			}
			status, _ := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/register", string(body))
			statuses <- status
		}()
	}
	<-started
	for range requests - 3 {
		status := <-statuses
		if status != http.StatusServiceUnavailable {
			t.Errorf("overflow registration status = %d, want 503", status)
		}
	}
	close(release)
	for range 3 {
		status := <-statuses
		if status != http.StatusCreated && status != http.StatusForbidden {
			t.Errorf("admitted registration status = %d, want 201 or 403", status)
		}
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("peak hashing work = %d, want at most 1", got)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("hash calls = %d, want exactly 3 admitted calls", got)
	}
}

func testPasswordWorkErrorsRemainStableLoginFailures(t *testing.T, st *store.Store) {
	work := newPasswordWorkWithOperations(1, 1, time.Second, passwordOperations{
		verify: func(string, string) (bool, error) { return false, errors.New("broken verifier") },
	})
	work.dummyHash = "dummy-hash"
	srv := newPasswordTestServer(t, st, work)
	status, body := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"wrong"}`)
	if status != http.StatusUnauthorized || body["error"] != "invalid credentials" {
		t.Fatalf("login verifier error: status=%d body=%v", status, body)
	}
}
