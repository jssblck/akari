package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthAttemptLimiterAppliesAccountAndSourceBudgets(t *testing.T) {
	now := time.Date(1843, time.December, 10, 12, 0, 0, 0, time.UTC)
	l := &authAttemptLimiter{
		accounts: newAttemptBuckets(60, 2, 10),
		sources:  newAttemptBuckets(60, 3, 10),
	}
	if !l.Allow("ada", "source-a", now) || !l.Allow("ada", "source-b", now) {
		t.Fatal("ordinary account burst was rejected")
	}
	if l.Allow("ada", "source-c", now) {
		t.Fatal("account budget allowed an attempt beyond its burst")
	}
	if !l.Allow("grace", "source-a", now) || !l.Allow("anna", "source-a", now) {
		t.Fatal("ordinary source burst was rejected")
	}
	if l.Allow("katherine", "source-a", now) {
		t.Fatal("source budget allowed an attempt beyond its burst")
	}
	if !l.Allow("ada", "source-a", now.Add(time.Second)) {
		t.Fatal("refilled budgets did not admit a later attempt")
	}
}

func TestAttemptBucketsBoundKeyMemory(t *testing.T) {
	now := time.Date(1906, time.December, 9, 0, 0, 0, 0, time.UTC)
	b := newAttemptBuckets(60, 1, 3)
	for _, key := range []string{"grace", "ada", "anna", "katherine", "margaret"} {
		if !b.allow(key, now) {
			t.Fatalf("first attempt for %q was rejected", key)
		}
		now = now.Add(time.Millisecond)
	}
	if got := len(b.byKey); got != 3 {
		t.Fatalf("tracked keys = %d, want bounded size 3", got)
	}
	if _, ok := b.byKey["grace"]; ok {
		t.Fatal("least-recently used key was not evicted")
	}
}

func TestRequestSourceUsesDirectPeer(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "192.0.2.10:4321"
	r.Header.Set("X-Forwarded-For", "203.0.113.8")
	if got := requestSource(r); got != "192.0.2.10" {
		t.Fatalf("requestSource = %q, want direct peer", got)
	}
}
