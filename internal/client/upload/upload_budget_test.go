package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

type uploadBudgetServer struct {
	mu sync.Mutex

	nextSession int
	bySource    map[string]int
	stored      map[int][]byte

	checkStarted chan struct{}
	putStarted   chan struct{}
	releasePuts  chan struct{}
	releaseOnce  sync.Once
	activePuts   int
	maxPuts      int
	totalPuts    int
}

func newUploadBudgetServer() *uploadBudgetServer {
	return &uploadBudgetServer{
		bySource:     map[string]int{},
		stored:       map[int][]byte{},
		checkStarted: make(chan struct{}, 16),
		putStarted:   make(chan struct{}, 16),
		releasePuts:  make(chan struct{}),
	}
}

func (s *uploadBudgetServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/ingest/session", s.announce)
	mux.HandleFunc("POST /api/v1/ingest/blobs/check", s.checkBlobs)
	mux.HandleFunc("PUT /api/v1/ingest/blob/{sha256}", s.putBlob)
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/chunk", s.chunk)
	return mux
}

func (s *uploadBudgetServer) announce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID string `json:"source_session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	id, ok := s.bySource[req.SourceID]
	if !ok {
		s.nextSession++
		id = s.nextSession
		s.bySource[req.SourceID] = id
	}
	stored := append([]byte(nil), s.stored[id]...)
	s.mu.Unlock()
	sum := sha256.Sum256(stored)
	writeJSON(w, map[string]any{
		"session_id":    id,
		"stored_bytes":  len(stored),
		"prefix_sha256": hex.EncodeToString(sum[:]),
	})
}

func (s *uploadBudgetServer) checkBlobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SHA256 []string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.checkStarted <- struct{}{}
	writeJSON(w, map[string]any{"missing": req.SHA256})
}

func (s *uploadBudgetServer) putBlob(w http.ResponseWriter, r *http.Request) {
	_ = readAll(r)
	s.mu.Lock()
	s.activePuts++
	s.totalPuts++
	if s.activePuts > s.maxPuts {
		s.maxPuts = s.activePuts
	}
	s.mu.Unlock()
	s.putStarted <- struct{}{}

	select {
	case <-s.releasePuts:
	case <-r.Context().Done():
		s.finishPut()
		return
	}
	s.finishPut()
	writeJSON(w, map[string]any{"sha256": r.PathValue("sha256")})
}

func (s *uploadBudgetServer) finishPut() {
	s.mu.Lock()
	s.activePuts--
	s.mu.Unlock()
}

func (s *uploadBudgetServer) releaseUploads() {
	s.releaseOnce.Do(func() { close(s.releasePuts) })
}

func (s *uploadBudgetServer) chunk(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body := readAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset != len(s.stored[id]) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "offset mismatch", "stored_bytes": len(s.stored[id])})
		return
	}
	s.stored[id] = append(s.stored[id], body...)
	writeJSON(w, map[string]any{"stored_bytes": len(s.stored[id]), "message_count": 1})
}

func budgetTarget(path, sourceID string) Target {
	return Target{Agent: "claude", Path: path, SourceID: sourceID, ProjectKey: "github.com/o/r", Machine: "m"}
}

func waitForBudgetSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForBudgetResult(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func TestUploadBudgetIsSharedAcrossConcurrentSyncs(t *testing.T) {
	setChunkTarget(t, 1<<30)
	backend := newUploadBudgetServer()
	t.Cleanup(backend.releaseUploads)
	server := httptest.NewServer(backend.handler())
	t.Cleanup(server.Close)
	c := New(server.Client(), server.URL, "test-token")
	const width = 3
	c.uploadSlots = make(chan struct{}, width)
	adaPath := tempFile(t, distinctBodySession(2, 40))
	gracePath := tempFile(t, distinctBodySession(2, 41))

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() {
		_, err := c.SyncFile(context.Background(), budgetTarget(adaPath, "ada"))
		errA <- err
	}()
	go func() {
		_, err := c.SyncFile(context.Background(), budgetTarget(gracePath, "grace"))
		errB <- err
	}()

	for range width {
		waitForBudgetSignal(t, backend.putStarted, "an upload slot")
	}
	backend.releaseUploads()
	if err := waitForBudgetResult(t, errA, "Ada's sync"); err != nil {
		t.Fatalf("sync Ada's session: %v", err)
	}
	if err := waitForBudgetResult(t, errB, "Grace's sync"); err != nil {
		t.Fatalf("sync Grace's session: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.maxPuts != width {
		t.Fatalf("peak concurrent uploads = %d, want shared budget width %d", backend.maxPuts, width)
	}
	if backend.totalPuts != 4 {
		t.Fatalf("uploaded %d bodies, want 4 across both sessions", backend.totalPuts)
	}
}

func TestUploadBudgetWaitHonorsCancellation(t *testing.T) {
	setChunkTarget(t, 1<<30)
	backend := newUploadBudgetServer()
	t.Cleanup(backend.releaseUploads)
	server := httptest.NewServer(backend.handler())
	t.Cleanup(server.Close)
	c := New(server.Client(), server.URL, "test-token")
	c.uploadSlots = make(chan struct{}, 1)
	adaPath := tempFile(t, distinctBodySession(1, 40))
	gracePath := tempFile(t, distinctBodySession(1, 41))

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.SyncFile(context.Background(), budgetTarget(adaPath, "ada"))
		firstDone <- err
	}()
	waitForBudgetSignal(t, backend.checkStarted, "the first existence check")
	waitForBudgetSignal(t, backend.putStarted, "the first upload")

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := c.SyncFile(ctx, budgetTarget(gracePath, "grace"))
		secondDone <- err
	}()
	waitForBudgetSignal(t, backend.checkStarted, "the second existence check")
	cancel()
	if err := waitForBudgetResult(t, secondDone, "the canceled sync"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sync error = %v, want context.Canceled", err)
	}

	backend.releaseUploads()
	if err := waitForBudgetResult(t, firstDone, "the first sync"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.totalPuts != 1 {
		t.Fatalf("started %d uploads, want only the slot holder", backend.totalPuts)
	}
}
