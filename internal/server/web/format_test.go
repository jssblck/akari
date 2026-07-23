package web

import "testing"

func TestFmtCost(t *testing.T) {
	cases := []struct {
		usd  float64
		want string
	}{
		{0, "$0.00"},
		{0.0042, "$0.0042"},
		{1.5, "$1.50"},
		{12.34, "$12.34"},
		{123.4, "$123.40"},
		{5925, "$5925.00"},
	}
	for _, c := range cases {
		if got := FmtCost(c.usd); got != c.want {
			t.Errorf("FmtCost(%v) = %q, want %q", c.usd, got, c.want)
		}
	}
}

func TestFmtPercent(t *testing.T) {
	cases := []struct {
		f    float64
		want string
	}{
		{0, "0%"},
		{-0.2, "0%"},
		{0.001, "1%"},
		{0.5, "50%"},
		{1, "100%"},
	}
	for _, c := range cases {
		if got := FmtPercent(c.f); got != c.want {
			t.Errorf("FmtPercent(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestFmtTokens(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{742, "742"},
		{1234, "1,234"},
		{1234567, "1,234,567"},
	}
	for _, c := range cases {
		if got := FmtTokens(c.n); got != c.want {
			t.Errorf("FmtTokens(%v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFmtTokensCompact(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{742, "742"},
		{12_300, "12.3k"},
		{3_400_000, "3.4M"},
		{2_500_000_000, "2.5B"},
	}
	for _, c := range cases {
		if got := FmtTokensCompact(c.n); got != c.want {
			t.Errorf("FmtTokensCompact(%v) = %q, want %q", c.n, got, c.want)
		}
	}
}
