package main

import (
	"errors"
	"testing"
)

func TestDeriveServerURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{":8080", "http://localhost:8080"},
		{"0.0.0.0:9000", "http://localhost:9000"},
		{"[::]:8080", "http://localhost:8080"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"example.test:443", "http://example.test:443"},
		{"garbage-without-a-port", "http://localhost:8080"}, // SplitHostPort fails -> safe fallback
	}
	for _, c := range cases {
		if got := deriveServerURL(c.listen); got != c.want {
			t.Errorf("deriveServerURL(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
}

func TestFinishDevSeed(t *testing.T) {
	sentinel := errors.New("boom")

	if err := finishDevSeed(nil, false); err != nil {
		t.Errorf("nil error, best-effort: got %v, want nil", err)
	}
	if err := finishDevSeed(nil, true); err != nil {
		t.Errorf("nil error, strict: got %v, want nil", err)
	}
	// Strict surfaces the failure unchanged.
	if err := finishDevSeed(sentinel, true); !errors.Is(err, sentinel) {
		t.Errorf("real error, strict: got %v, want %v", err, sentinel)
	}
	// Best-effort downgrades a failure to nil (logged) so a post-start hook never
	// blocks `eph up`.
	if err := finishDevSeed(sentinel, false); err != nil {
		t.Errorf("real error, best-effort: got %v, want nil", err)
	}
}
