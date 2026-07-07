package web

import (
	"fmt"
	"strings"

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
	// Tree is the whole-work-item rollup (own cost plus every descendant subagent), the
	// same fold the feed's fan-out chip reads, so the audit header names what the whole
	// run cost and not just the root turn.
	Tree store.TreeRollup
	// Models is the session's serving models, heaviest first, for the audit header line.
	Models []string
	DupIDs int
}

// SetPage installs a transcript window on the view and groups the page's
// snapshot-pinned tools and attachments for the row renderer.
func (v *SessionView) SetPage(page store.TranscriptPage) {
	v.Page = page
	v.WindowTools = ToolsByOrdinal(page.Tools)
	v.WindowAttachments = AttachmentsByOrdinal(page.Attachments)
}

// SubagentFailures counts the direct children that ended in an error, for the collapsed
// fold's "N failed" clause and the audit header's wasted-spend note.
func SubagentFailures(subs []store.SubagentRow) int {
	n := 0
	for _, s := range subs {
		if s.Failed() {
			n++
		}
	}
	return n
}

// SessionWasted is the session's wasted spend, mirroring the Overview verdict's
// "wasted on failed runs" figure at single-work-item scale: the session's own cost when
// it errored or was abandoned, plus the cost of every direct subagent that errored. The
// note names where the waste came from; both are zero-valued for a healthy run, so the
// header shows no waste tile at all.
func SessionWasted(d store.SessionDetail, sig store.SessionSignals, subs []store.SubagentRow) (usd float64, incomplete bool, note string) {
	var parts []string
	if sig.Outcome == "errored" || sig.Outcome == "abandoned" {
		usd += d.TotalCostUSD
		incomplete = incomplete || d.CostIncomplete
		parts = append(parts, "this run "+sig.Outcome)
	}
	var failed int
	for _, s := range subs {
		if s.Failed() {
			failed++
			usd += s.TotalCostUSD
			incomplete = incomplete || s.CostIncomplete
		}
	}
	if failed > 0 {
		unit := "subagents"
		if failed == 1 {
			unit = "subagent"
		}
		parts = append(parts, fmt.Sprintf("%d failed %s", failed, unit))
	}
	return usd, incomplete, strings.Join(parts, " + ")
}

// VerdictOutcomeTone bands a session outcome onto the status palette for the audit
// header's verdict tile: completed green, abandoned peach, errored rose, unknown
// toneless.
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

// VerdictValueClass is the class for a verdict tile's figure, adding a tone modifier
// when the figure carries a banded verdict (the outcome word) and plain otherwise (cost,
// duration). The modifier maps to the status palette in session.css.
func VerdictValueClass(tone string) string {
	if tone == "" {
		return "vvalue"
	}
	return "vvalue tone-" + tone
}

// VerdictHeadline is the audit header's leading figure: the letter grade with the
// outcome word for a scored session ("B · completed"), the outcome alone when the
// session settled without a score, and a plain dash while the verdict is still pending
// so the tile never invents a judgement.
func VerdictHeadline(s store.SessionSignals) string {
	if s.Scored() {
		return *s.Grade + " · " + strings.ToLower(OutcomeLabel(s.Outcome))
	}
	if s.Outcome != "" && s.Outcome != "unknown" {
		return strings.ToLower(OutcomeLabel(s.Outcome))
	}
	return "-"
}

// VerdictSub is the verdict tile's second line: the score for a scored session, and the
// C3 fix for everything else, naming that grading happens at settle instead of leaving
// a bare dash that reads as missing data.
func VerdictSub(s store.SessionSignals) string {
	if s.Scored() {
		return fmt.Sprintf("scored %d / 100", *s.Score)
	}
	return "graded after the session settles"
}

// ScoreArithmeticLine spells out the grade the way the quality tooltip does, promoted to
// a visible line under the verdict strip: 100 minus each penalty, equals the score. A
// clean scored run reads "100, no penalties"; an unscored session returns "" so the line
// does not render.
func ScoreArithmeticLine(s store.SessionSignals) string {
	if !s.Scored() {
		return ""
	}
	items := ScoreBreakdownItems(s)
	if len(items) == 0 {
		return "100, no penalties"
	}
	var b strings.Builder
	b.WriteString("100")
	for _, it := range items {
		fmt.Fprintf(&b, " - %d (%s)", it.Points, it.Label)
	}
	fmt.Fprintf(&b, " = %d", *s.Score)
	return b.String()
}

// WorkItemNote is the Cost tile's second line: the whole-work-item cost and fan-out size
// when the session delegated work ("work item $6.12 across 34 subagents"), or "" for a
// session that spawned nothing, whose own cost already tells the whole story.
func WorkItemNote(tree store.TreeRollup) string {
	if tree.SubagentCount == 0 {
		return ""
	}
	unit := "subagents"
	if tree.SubagentCount == 1 {
		unit = "subagent"
	}
	return fmt.Sprintf("work item %s across %d %s", FmtCost(tree.CostUSD, tree.CostIncomplete), tree.SubagentCount, unit)
}

// ModelsLine joins the session's serving models for the Duration tile's second line,
// naming at most three and folding the rest into a count so the line stays one line.
func ModelsLine(models []string) string {
	if len(models) == 0 {
		return ""
	}
	if len(models) > 3 {
		return strings.Join(models[:3], ", ") + fmt.Sprintf(" +%d more", len(models)-3)
	}
	return strings.Join(models, ", ")
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
