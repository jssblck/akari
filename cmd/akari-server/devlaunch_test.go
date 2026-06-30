package main

import (
	"reflect"
	"testing"
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
