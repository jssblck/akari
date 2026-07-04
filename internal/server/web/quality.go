// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// HeaderStats bundles the derived stat-tile inputs the session instrument header
// renders beside the token figures: prompt-cache effectiveness and the session's
// quality signals. Threading one struct keeps the SessionMain / sessionStats / public
// render seams stable as the header grows, rather than widening every signature each
// time a new tile lands.
type HeaderStats struct {
	Cache   store.CacheStats
	Signals store.SessionSignals
	// Fallbacks is the session's recorded model fallbacks, loaded only when the
	// session's rollup says it has any (ModelFallbackCount > 0), so the common
	// no-fallback session pays for no extra read. The header tile and the transcript
	// notices both read it, so bundling it here keeps the render seams stable rather
	// than widening every signature the way a bare slice parameter would.
	Fallbacks []store.ModelFallback
	// Thinking is the session's observed-thinking readout: the absolute band and the
	// estimated per-turn token volumes behind it, all read straight from the stored signals
	// row (the band is an absolute cut, not a cohort rank, so no extra read is needed).
	// Bundled here for the same stable-seams reason as Fallbacks.
	Thinking ThinkingReadout
}

// QualityGrade is the headline of the session Quality tile: the letter grade for a
// scored session, or a plain dash for an unscored one (an unknown outcome with no tool
// signal, where a letter would invent a verdict the transcript does not support).
func QualityGrade(s store.SessionSignals) string {
	if s.Scored() {
		return *s.Grade
	}
	return "-"
}

// QualityGradeClass is the CSS modifier for the Quality tile, banding its colour the
// way a report card reads: A and B good, C watch, D and F poor, an unscored session
// neutral. It maps to the status palette already in the sheet (sage, peach, rose)
// rather than introducing new hues, keeping to the One Voice Rule.
func QualityGradeClass(s store.SessionSignals) string {
	if !s.Scored() {
		return "q-none"
	}
	switch *s.Grade {
	case "A", "B":
		return "q-good"
	case "C":
		return "q-watch"
	default: // D, F
		return "q-poor"
	}
}

// OutcomeLabel renders a stored outcome string title-cased for display. An empty or
// unrecognized value reads "Unknown", so the tile never shows a blank cell.
func OutcomeLabel(outcome string) string {
	switch outcome {
	case "completed":
		return "Completed"
	case "abandoned":
		return "Abandoned"
	case "errored":
		return "Errored"
	default:
		return "Unknown"
	}
}

// QualityScoreLabel renders the numeric score for the Quality tooltip: "n / 100" for a
// scored session, "not scored" otherwise, so an unscored session reads as a deliberate
// abstention rather than a zero.
func QualityScoreLabel(s store.SessionSignals) string {
	if !s.Scored() {
		return "not scored"
	}
	return fmt.Sprintf("%d / 100", *s.Score)
}

// PeakContextLabel renders the heaviest single-turn context the session reached, in full
// tokens, for the Tokens tooltip. It is a window-independent measure of how loaded the
// session got. An unmeasured session (no usage) reads a dash rather than a misleading zero.
func PeakContextLabel(s store.SessionSignals) string {
	if s.PeakContextTokens == nil {
		return "-"
	}
	return FmtTokens(*s.PeakContextTokens)
}

// ContextResetsLabel renders how many inferred context resets (compactions or clears) the
// session went through, for the Tokens tooltip. An unmeasured session reads a dash; a
// measured session with none reads "0", a real "it never shed context".
func ContextResetsLabel(s store.SessionSignals) string {
	if s.ContextResetCount == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *s.ContextResetCount)
}
