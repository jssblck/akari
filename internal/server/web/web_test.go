package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestDuplicateIDsLabel(t *testing.T) {
	if got := DuplicateIDsLabel(1); got != "1 duplicate id" {
		t.Errorf("label(1) = %q", got)
	}
	if got := DuplicateIDsLabel(3); got != "3 duplicate ids" {
		t.Errorf("label(3) = %q", got)
	}
}

// TestSessionPageShowsDuplicateIDChip renders the session header with a non-zero and
// a zero duplicate-id count and confirms the warning chip appears only for the
// former, so a reader is told a session replayed an id (and a malformed reuse would
// not stay hidden). The count is a scalar the handler passes in, computed in the
// store rather than over the in-memory tool calls.
func TestSessionPageShowsDuplicateIDChip(t *testing.T) {
	p := Page{Title: "Session", LoggedIn: true, Username: "Grace Hopper"}
	d := store.SessionDetail{}
	d.ID = 7
	d.Agent = "claude"
	msgs := []store.Message{{Ordinal: 0, Role: "assistant"}, {Ordinal: 1, Role: "assistant"}}
	tools := map[int][]store.ToolCallView{
		0: {{MessageOrdinal: 0, ToolName: "Read"}},
		1: {{MessageOrdinal: 1, ToolName: "Read"}},
	}

	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 1, false, false))
	if !strings.Contains(html, "1 duplicate id") {
		t.Error("session page should show the duplicate-id chip when the count is non-zero")
	}

	html = renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 0, false, false))
	if strings.Contains(html, "duplicate id") {
		t.Error("session page should not show the chip when the count is zero")
	}
}
