package config

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in       string
		fallback time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{"", time.Hour, time.Hour, false},
		{"30m", time.Hour, 30 * time.Minute, false},
		{"0", time.Hour, 0, false},
		{"  2h  ", time.Hour, 2 * time.Hour, false},
		{"-5m", time.Hour, 0, true},
		{"banana", time.Hour, 0, true},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in, c.fallback)
		if (err != nil) != c.wantErr {
			t.Errorf("parseDuration(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
