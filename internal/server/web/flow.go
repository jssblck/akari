package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// The flow ribbon (D-4) renders one tick per turn above the transcript, colored by what
// the turn did, so a reviewer sees the session's shape (a long failure streak, an edit
// churn loop, a wall of runs) before reading a word. It is presentation only: it reads
// the same bounded outline rows and tool metadata the rail already holds.

// flowRibbonMinMessages gates the ribbon: below it a session's shape is visible by
// scrolling, and a strip of a few ticks carries nothing.
const flowRibbonMinMessages = 12

// FlowRibbonVisible reports whether the session is long enough for its ribbon to carry
// information.
func FlowRibbonVisible(msgs []store.Message) bool {
	return len(msgs) >= flowRibbonMinMessages
}

// FlowTickClass maps one turn to its tick's tone. Failure outranks everything (a turn
// that both edited and failed reads as a failure); then edits, then runs, so the ribbon
// reads "where work landed and where it broke". A user turn reads as its own segment
// mark, so the ribbon's rhythm shows the prompt cadence; context injections and plain
// assistant turns stay faint.
func FlowTickClass(m store.Message, steps []store.ToolCallView) string {
	var edit, run bool
	for _, t := range steps {
		if t.ResultStatus == "error" {
			return "ft-fail"
		}
		switch t.Category {
		case "edit", "write":
			edit = true
		case "bash":
			run = true
		}
	}
	switch {
	case edit:
		return "ft-edit"
	case run:
		return "ft-run"
	case m.Role == "user":
		return "ft-user"
	default:
		return "ft-plain"
	}
}

// FlowTickTitle is a tick's hover label: the turn number and role, its outline title
// when one reads, and its tool tally with failures broken out, so hovering the ribbon
// scrubs through the session without leaving it.
func FlowTickTitle(m store.Message, steps []store.ToolCallView) string {
	label := fmt.Sprintf("#%d %s", m.Ordinal, m.Role)
	if t := OutlineTitle(m); t != "" && m.Role != "assistant" {
		label += ": " + t
	}
	if len(steps) > 0 {
		failed := 0
		for _, s := range steps {
			if s.ResultStatus == "error" {
				failed++
			}
		}
		label += fmt.Sprintf(" · %d tools", len(steps))
		if failed > 0 {
			label += fmt.Sprintf(", %d failed", failed)
		}
	}
	return label
}
