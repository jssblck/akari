package durationfmt

import (
	"testing"
	"time"
)

func TestPositive(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},                    // a zero-length span is a real 0s, not a dash
		{30 * time.Second, "30s"},    //
		{59 * time.Second, "59s"},    //
		{90 * time.Second, "1m30s"},  // minutes carry their remaining seconds
		{60 * time.Second, "1m0s"},   // a whole minute keeps the 0s
		{100 * time.Minute, "1h40m"}, // hours carry their remaining minutes
		{2 * time.Hour, "2h0m"},      // a whole hour keeps the 0m
		{25 * time.Hour, "25h0m"},    // no day rollover: hours keep climbing
		{90 * time.Minute, "1h30m"},  //
	}
	for _, c := range cases {
		if got := Positive(c.d); got != c.want {
			t.Errorf("Positive(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSpan(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	ptr := func(tm time.Time) *time.Time { return &tm }
	cases := []struct {
		name       string
		start, end *time.Time
		want       string
	}{
		{"measured span", ptr(base), ptr(base.Add(90 * time.Second)), "1m30s"},
		{"zero-length span renders 0s", ptr(base), ptr(base), "0s"},
		{"missing start", nil, ptr(base), "-"},
		{"missing end", ptr(base), nil, "-"},
		{"zero-value start", ptr(time.Time{}), ptr(base), "-"},
		{"end before start", ptr(base.Add(time.Hour)), ptr(base), "-"},
	}
	for _, c := range cases {
		if got := Span(c.start, c.end); got != c.want {
			t.Errorf("%s: Span = %q, want %q", c.name, got, c.want)
		}
	}
}
