package store

import "testing"

// TestPanelWorkers pins the Insights panel-dispatch sizing that guards the pool deadlock: a
// pool that cannot spare a connection beyond the one the control transaction holds returns 0,
// which routes Insights to run the panels sequentially on the control connection rather than
// have concurrent panels block forever on Acquire. Any larger pool returns the spare-connection
// count, capped at insightsPanelLimit.
func TestPanelWorkers(t *testing.T) {
	cases := []struct {
		maxConns, want int
	}{
		{0, 0}, // a degenerate pool: nothing spare, run sequentially
		{1, 0}, // the control connection is the whole pool, so no panel could acquire another
		{2, 1}, // one spare beyond the control connection
		{3, 2},
		{insightsPanelLimit, insightsPanelLimit - 1}, // control takes one of the limit's own slots
		{insightsPanelLimit + 1, insightsPanelLimit}, // spare reaches the cap
		{100, insightsPanelLimit},                    // spare far exceeds the cap, so the cap wins
	}
	for _, c := range cases {
		if got := panelWorkers(c.maxConns); got != c.want {
			t.Errorf("panelWorkers(%d) = %d, want %d", c.maxConns, got, c.want)
		}
	}
}
