package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestDuplicateToolCallIDs(t *testing.T) {
	cases := []struct {
		name  string
		tools map[int][]store.ToolCallView
		want  int
	}{
		{"empty", nil, 0},
		{
			"all unique",
			map[int][]store.ToolCallView{
				0: {{CallUID: "a"}},
				1: {{CallUID: "b"}, {CallUID: "c"}},
			},
			0,
		},
		{
			// One id repeated across two turns (the resume/compaction replay).
			"one duplicate id",
			map[int][]store.ToolCallView{
				0: {{CallUID: "dup"}},
				1: {{CallUID: "dup"}},
				2: {{CallUID: "other"}},
			},
			1,
		},
		{
			// Two distinct ids each repeated: the count is of colliding ids, not rows.
			"two duplicate ids",
			map[int][]store.ToolCallView{
				0: {{CallUID: "x"}, {CallUID: "y"}},
				1: {{CallUID: "x"}, {CallUID: "y"}},
			},
			2,
		},
		{
			// An empty id (an unkeyed call) is never counted as a duplicate.
			"blank ids ignored",
			map[int][]store.ToolCallView{
				0: {{CallUID: ""}},
				1: {{CallUID: ""}},
			},
			0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DuplicateToolCallIDs(c.tools); got != c.want {
				t.Fatalf("DuplicateToolCallIDs = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDuplicateIDsLabel(t *testing.T) {
	if got := DuplicateIDsLabel(1); got != "1 duplicate id" {
		t.Errorf("label(1) = %q", got)
	}
	if got := DuplicateIDsLabel(3); got != "3 duplicate ids" {
		t.Errorf("label(3) = %q", got)
	}
}

// TestSessionPageShowsDuplicateIDChip renders the session header with and without a
// repeated tool-call id and confirms the warning chip appears only when one is
// present, so a reader is told a session replayed an id (and a malformed reuse would
// not stay hidden).
func TestSessionPageShowsDuplicateIDChip(t *testing.T) {
	p := Page{Title: "Session", LoggedIn: true, Username: "Grace Hopper"}
	d := store.SessionDetail{}
	d.ID = 7
	d.Agent = "claude"
	msgs := []store.Message{{Ordinal: 0, Role: "assistant"}, {Ordinal: 1, Role: "assistant"}}

	withDup := map[int][]store.ToolCallView{
		0: {{MessageOrdinal: 0, ToolName: "Read", CallUID: "dup"}},
		1: {{MessageOrdinal: 1, ToolName: "Read", CallUID: "dup"}},
	}
	html := renderComponent(t, SessionPage(p, d, msgs, withDup, nil, nil, false, false))
	if !strings.Contains(html, "1 duplicate id") {
		t.Error("session page should show the duplicate-id chip when an id repeats")
	}

	noDup := map[int][]store.ToolCallView{
		0: {{MessageOrdinal: 0, ToolName: "Read", CallUID: "a"}},
		1: {{MessageOrdinal: 1, ToolName: "Read", CallUID: "b"}},
	}
	html = renderComponent(t, SessionPage(p, d, msgs, noDup, nil, nil, false, false))
	if strings.Contains(html, "duplicate id") {
		t.Error("session page should not show the chip when all ids are unique")
	}
}
