package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEphEnv(t *testing.T) {
	// A real `eph env -f json` payload after `eph up postgres`: the Postgres-backed
	// URLs resolve, but AKARI_URL still points at the eph server service we do not
	// start, so it keeps an unresolved ${server.port} placeholder.
	in := []byte(`{
		"AKARI_DATABASE_URL": "postgres://akari:akari@localhost:55001/akari?sslmode=disable",
		"AKARI_TEST_DATABASE_URL": "postgres://akari:akari@localhost:55001/akari_test?sslmode=disable",
		"AKARI_COOKIE_INSECURE": "1",
		"AKARI_URL": "http://localhost:${server.port}"
	}`)
	want := map[string]string{
		"AKARI_DATABASE_URL":      "postgres://akari:akari@localhost:55001/akari?sslmode=disable",
		"AKARI_TEST_DATABASE_URL": "postgres://akari:akari@localhost:55001/akari_test?sslmode=disable",
		"AKARI_COOKIE_INSECURE":   "1",
	}

	got, err := parseEphEnv(in)
	if err != nil {
		t.Fatalf("parseEphEnv: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEphEnv dropped the wrong keys:\n got %v\nwant %v", got, want)
	}
}

func TestParseEphEnvInvalid(t *testing.T) {
	if _, err := parseEphEnv([]byte("not json")); err == nil {
		t.Error("parseEphEnv(invalid json): want error, got nil")
	}
}

func TestWaitHealthyBecomesHealthy(t *testing.T) {
	var probes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Report unhealthy for the first two probes, healthy after that, so the
		// retry loop has to poll more than once.
		if atomic.AddInt32(&probes, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := waitHealthy(context.Background(), srv.Client(), srv.URL, 10, time.Millisecond); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if got := atomic.LoadInt32(&probes); got < 3 {
		t.Errorf("expected at least 3 probes before healthy, got %d", got)
	}
}

func TestWaitHealthyTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := waitHealthy(context.Background(), srv.Client(), srv.URL, 3, time.Millisecond)
	if err == nil {
		t.Fatal("waitHealthy: want a timeout error after exhausting attempts, got nil")
	}
}

func TestWaitHealthyContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Cancel up front: the first failed probe should bail on ctx.Done() rather
	// than wait out the (here, hour-long) interval, so teardown is not blocked.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitHealthy(ctx, srv.Client(), srv.URL, 100, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitHealthy: want context.Canceled, got %v", err)
	}
}
