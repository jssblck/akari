package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// SessionView bundles everything the session page and its live body fragment render, so
// the SessionPage / SessionMain / fragment seams stay stable as the auditor view grows
// (the same reasoning as HeaderStats, one level up). The handlers fill it; the templates
// only read it.
type SessionView struct {
	Detail store.SessionDetail
	// Outline is the whole session at bounded columns (store.OutlineMessages): one entry
	// per turn for the outline rail and the flow ribbon, regardless of how much of the
	// transcript the window below actually renders.
	Outline []store.Message
	// Page is the transcript window the body renders: the tail of the session on first
	// load, a windowed continuation on a "Show earlier" or live-append fetch.
	Page store.TranscriptPage
	// Tools is the whole session's tool metadata, for the outline rail and the flow
	// ribbon (which cover every turn regardless of the window below).
	Tools map[int][]store.ToolCallView
	// WindowTools and WindowAttachments are Page's own tool calls and attachments,
	// grouped by ordinal. Transcript rows render from these, never from Tools: they were
	// read in the same transaction as the rows, so a rebuild committing mid-request
	// cannot pair one projection's messages with another's chips or images.
	WindowTools       map[int][]store.ToolCallView
	WindowAttachments map[int][]store.AttachmentView
	Subagents         []store.SubagentRow
	Header            HeaderStats
	DupIDs            int
	// ProjectionRevision is the public pagination snapshot token. Private session
	// pagination reconciles through its live append cursor and leaves this zero.
	ProjectionRevision int64
}

// SetPage installs a transcript window on the view and groups the page's
// snapshot-pinned tools and attachments for the row renderer.
func (v *SessionView) SetPage(page store.TranscriptPage) {
	v.Page = page
	v.WindowTools = ToolsByOrdinal(page.Tools)
	v.WindowAttachments = AttachmentsByOrdinal(page.Attachments)
}

// SubagentFailures counts the direct children that ended in an error, for the collapsed
// fold's "N failed" clause.
func SubagentFailures(subs []store.SubagentRow) int {
	n := 0
	for _, s := range subs {
		if s.Failed() {
			n++
		}
	}
	return n
}

// VerdictOutcomeTone bands a session outcome onto the status palette for the subagents
// table's outcome column: completed green, abandoned peach, errored rose, unknown
// toneless. It is the single-session shape of CompletionTone.
func VerdictOutcomeTone(outcome string) string {
	switch outcome {
	case "completed":
		return "ok"
	case "abandoned":
		return "warn"
	case "errored":
		return "err"
	default:
		return ""
	}
}

// EarlierCountLabel is the "Show earlier" bar's second half, naming how much of the
// transcript stays unrendered above the window.
func EarlierCountLabel(n int) string {
	if n == 1 {
		return "1 earlier message"
	}
	return fmt.Sprintf("%d earlier messages", n)
}

// SubagentTitle is the label a subagents-table row leads with: the child's stripped
// first prompt (for an Agent-tool child this is the task description the parent gave
// it), falling back to the bare agent name so a row is never blank.
func SubagentTitle(r store.SubagentRow) string {
	if t := StripPromptPreamble(r.Title); t != "" {
		return t
	}
	return r.Agent + " subagent"
}
