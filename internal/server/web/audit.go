package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// FmtCompletionRate renders a 0..100 percentage for the verdict strip. A negative rate
// is the store's "nothing settled yet" sentinel, dashed rather than printed as a real 0%
// or 100% that would misread an empty cohort as a total failure or a clean sweep. A real
// rate reads as a whole-number percent, the resolution a headline tile wants.
func FmtCompletionRate(rate float64) string {
	if rate < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", rate)
}

// FmtGPA renders a grade-point average on the A=4..F=0 scale for the Quality tile. A
// negative GPA is the store's "no graded session in scope" sentinel, dashed rather than
// printed as 0.0, which would read as a fleet of straight F's. A real average shows one
// decimal, matching how the Insights GPA line reads at a glance.
func FmtGPA(gpa float64) string {
	if gpa < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", gpa)
}

// AttentionReasonLabel maps an attention row's canonical reason key to the short phrase
// the shortlist shows. The store ranks and keys the row; the label lives here so the two
// layers stay decoupled, the same split LabeledCount uses. An unknown key (none expected,
// since the store emits a fixed set) falls back to the raw key rather than an empty chip.
func AttentionReasonLabel(r store.AttentionRow) string {
	switch r.Reason {
	case "errored":
		return "errored"
	case "abandoned":
		return "abandoned"
	case "grade-f":
		return "graded F"
	case "grade-d":
		return "graded D"
	case "costly":
		return "costly run"
	default:
		return r.Reason
	}
}

// AttentionReasonTitle is the hover explanation for a reason chip: a full sentence saying
// why the run was flagged, so the terse chip label reads at a glance while the detail is
// one hover away. It pairs with AttentionReasonLabel over the same key set.
func AttentionReasonTitle(r store.AttentionRow) string {
	switch r.Reason {
	case "errored":
		return "This run ended in an error."
	case "abandoned":
		return "This run was left unfinished."
	case "grade-f", "grade-d":
		return "This run scored a low quality grade."
	case "costly":
		return "This run cost several times a typical run in this window."
	default:
		return ""
	}
}

// AttentionReasonTone maps a reason key to the tone class its chip wears, so a hard
// failure (errored, abandoned) reads in the error colour and a softer flag (a low grade
// or an expensive run) reads in the warn colour. It reuses the status palette the tags
// already carry rather than inventing a third scale.
func AttentionReasonTone(r store.AttentionRow) string {
	switch r.Reason {
	case "errored", "abandoned":
		return "err"
	default:
		return "warn"
	}
}

// CompletionTone bands the completion rate onto the status palette so the Completed
// tile's figure reads its own verdict at a glance: a fleet finishing most of what it
// starts reads green, a middling one peach, a mostly-failing one rose. An unmeasured
// rate (nothing settled) carries no tone, so the dash stays neutral rather than
// implying a judgement the data cannot support.
func CompletionTone(au store.AuditSummary) string {
	r := au.CompletionRate()
	switch {
	case r < 0:
		return ""
	case r >= 80:
		return "ok"
	case r >= 50:
		return "warn"
	default:
		return "err"
	}
}

// GPATone bands the GPA onto the same status palette as the completion rate, so the
// Quality tile's number reads its own verdict: a B average or better reads green, a
// C-ish average peach, below that rose. An unmeasured GPA carries no tone.
func GPATone(au store.AuditSummary) string {
	g := au.GPA()
	switch {
	case g < 0:
		return ""
	case g >= 3.0:
		return "ok"
	case g >= 2.0:
		return "warn"
	default:
		return "err"
	}
}

// VerdictValueClass is the class for a verdict tile's figure, adding a tone modifier
// when the figure carries a banded verdict (completion, quality) and plain otherwise
// (work count, spend). The modifier maps to the status palette in overview.css.
func VerdictValueClass(tone string) string {
	if tone == "" {
		return "vvalue"
	}
	return "vvalue tone-" + tone
}

// AttentionProjectLabel is the project a shortlisted session ran in, its display name
// falling back to the remote key when unnamed, so a row always names where the work
// happened. It mirrors SessionRowProject's fallback for the Sessions feed so a session
// reads under the same project label on both surfaces.
func AttentionProjectLabel(r store.AttentionRow) string {
	if r.ProjectName != "" {
		return r.ProjectName
	}
	return r.ProjectKey
}
