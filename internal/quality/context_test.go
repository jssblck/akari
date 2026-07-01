package quality

import "testing"

// TestContextHealth pins the peak and the inferred-reset rule. Peak is the largest
// single-turn context regardless of where it falls in the session; a reset is a sharp
// drop (to at most half the prior turn) from a context that was already substantial, so
// shallow dips and drops from a small early context do not count. The input is the session's
// per-turn prompt sizes in transcript order, the same shape the store gathers.
func TestContextHealth(t *testing.T) {
	cases := []struct {
		name       string
		perTurn    []int64
		wantPeak   int64
		wantResets int
	}{
		{
			name:       "no turns measures nothing",
			perTurn:    nil,
			wantPeak:   0,
			wantResets: 0,
		},
		{
			name:       "a single turn is its own peak with no reset",
			perTurn:    []int64{50000},
			wantPeak:   50000,
			wantResets: 0,
		},
		{
			name:       "monotonic growth never resets",
			perTurn:    []int64{10000, 30000, 80000, 150000},
			wantPeak:   150000,
			wantResets: 0,
		},
		{
			name: "one compaction is one reset and the peak is the pre-drop high",
			// Context climbs to 180k, sheds to 20k (a compaction), then regrows. The peak
			// stays 180k even though the last turn is far smaller.
			perTurn:    []int64{50000, 120000, 180000, 20000, 60000},
			wantPeak:   180000,
			wantResets: 1,
		},
		{
			name:       "two compactions count twice",
			perTurn:    []int64{100000, 180000, 15000, 90000, 160000, 10000},
			wantPeak:   180000,
			wantResets: 2,
		},
		{
			name: "a drop from a small early context is not a reset",
			// The prior turn is below the keep floor, so the fall reads as early warm-up
			// noise rather than a context being shed.
			perTurn:    []int64{10000, 3000},
			wantPeak:   10000,
			wantResets: 0,
		},
		{
			name: "a shallow dip is not a reset",
			// A quarter drop leaves more than half the context, below the reset threshold.
			perTurn:    []int64{100000, 120000, 90000},
			wantPeak:   120000,
			wantResets: 0,
		},
		{
			name: "a drop to exactly half counts",
			// The threshold is inclusive: falling to exactly half the prior turn is a reset.
			perTurn:    []int64{40000, 20000},
			wantPeak:   40000,
			wantResets: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peak, resets := ContextHealth(tc.perTurn)
			if peak != tc.wantPeak {
				t.Errorf("peak = %d, want %d", peak, tc.wantPeak)
			}
			if resets != tc.wantResets {
				t.Errorf("resets = %d, want %d", resets, tc.wantResets)
			}
		})
	}
}
