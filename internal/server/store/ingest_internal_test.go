package store

import "testing"

// TestSubagentParentSource pins how a child transcript's source id yields its parent's.
// The client nests a Claude subagent under the spawning session as
// "<parent>/subagents/...", so the parent id is the prefix before the first marker. Only
// Claude nests this way, and a marker at the very start has no parent before it.
func TestSubagentParentSource(t *testing.T) {
	cases := []struct {
		name       string
		sourceID   string
		wantParent string
		wantOK     bool
	}{
		{"top-level claude", "5d80166f-12d5-4bf3-a45f-c80d52fdfe81", "", false},
		{"codex flat id", "rollout-2026-01-02T10-00-00-abc", "", false},
		{"task subagent", "5d80166f/subagents/agent-a6d63783941853397", "5d80166f", true},
		{"workflow journal", "5d80166f/subagents/workflows/wf_abc/journal", "5d80166f", true},
		{"marker at start has no parent", "/subagents/agent-x", "", false},
		{"nested subagent links to the top ancestor", "main/subagents/agent-x/subagents/agent-y", "main", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent, ok := subagentParentSource(c.sourceID)
			if ok != c.wantOK || parent != c.wantParent {
				t.Fatalf("subagentParentSource(%q) = (%q, %v), want (%q, %v)",
					c.sourceID, parent, ok, c.wantParent, c.wantOK)
			}
		})
	}
}
