package quality

import "testing"

// TestClassify pins the outcome rule and the precedence between its cases. The
// ordering matters: a session that ends badly must not read "completed" merely
// because the assistant happened to speak last, so the strong signals (an errored
// tail, a settled mid-tool ending) are checked before the last-word heuristic. The
// table also pins v2's new resolutions: a settled automation run reads as completed,
// and an idle mid-tool ending reads as errored (automation) or abandoned (human)
// rather than staying unknown.
func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		facts   Facts
		outcome Outcome
		conf    Confidence
	}{
		{
			name:    "trailing failures win over a pending tool call",
			facts:   Facts{UserMessages: 2, LastAssistantOrd: 9, LastUserOrd: 4, ToolCallPending: true, TrailingFailures: 3, IdleLongEnough: true},
			outcome: OutcomeErrored, conf: ConfHigh,
		},
		{
			name:    "pending tool call on a still-recent session holds the verdict",
			facts:   Facts{UserMessages: 2, LastAssistantOrd: 9, LastUserOrd: 4, ToolCallPending: true, IdleLongEnough: false},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
		{
			name:    "idle mid-tool automation run is errored at medium confidence",
			facts:   Facts{UserMessages: 0, LastAssistantOrd: 6, LastUserOrd: -1, ToolCallPending: true, IdleLongEnough: true},
			outcome: OutcomeErrored, conf: ConfMedium,
		},
		{
			name:    "idle mid-tool human session is abandoned at medium confidence",
			facts:   Facts{UserMessages: 3, LastAssistantOrd: 6, LastUserOrd: 10, ToolCallPending: true, IdleLongEnough: true},
			outcome: OutcomeAbandoned, conf: ConfMedium,
		},
		{
			name:    "settled automation with a substantive assistant word is completed at medium confidence",
			facts:   Facts{UserMessages: 0, LastAssistantOrd: 5, LastUserOrd: -1, IdleLongEnough: true},
			outcome: OutcomeCompleted, conf: ConfMedium,
		},
		{
			name:    "automation not yet idle stays unknown so a live subagent is not graded early",
			facts:   Facts{UserMessages: 0, LastAssistantOrd: 5, LastUserOrd: -1, IdleLongEnough: false},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
		{
			name:    "settled automation with no assistant word is unknown",
			facts:   Facts{UserMessages: 0, LastAssistantOrd: -1, LastUserOrd: -1, IdleLongEnough: true},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
		{
			name:    "errored tail wins over a trailing assistant message",
			facts:   Facts{UserMessages: 2, LastAssistantOrd: 9, LastUserOrd: 4, TrailingFailures: 3},
			outcome: OutcomeErrored, conf: ConfHigh,
		},
		{
			name:    "two trailing failures is not yet errored",
			facts:   Facts{UserMessages: 2, LastAssistantOrd: 9, LastUserOrd: 4, TrailingFailures: 2},
			outcome: OutcomeCompleted, conf: ConfHigh,
		},
		{
			name:    "only tool plumbing, no substantive turn, is unknown",
			facts:   Facts{UserMessages: 1, LastAssistantOrd: -1, LastUserOrd: -1},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
		{
			name:    "assistant had the last substantive word is completed",
			facts:   Facts{UserMessages: 3, LastAssistantOrd: 12, LastUserOrd: 8},
			outcome: OutcomeCompleted, conf: ConfHigh,
		},
		{
			name:    "user spoke last and went quiet long enough is abandoned",
			facts:   Facts{UserMessages: 3, LastAssistantOrd: 6, LastUserOrd: 10, IdleLongEnough: true},
			outcome: OutcomeAbandoned, conf: ConfMedium,
		},
		{
			name:    "user spoke last but still recent is unknown, not abandoned",
			facts:   Facts{UserMessages: 3, LastAssistantOrd: 6, LastUserOrd: 10, IdleLongEnough: false},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotOutcome, gotConf := Classify(c.facts)
			if gotOutcome != c.outcome || gotConf != c.conf {
				t.Errorf("Classify(%+v) = (%s, %s), want (%s, %s)",
					c.facts, gotOutcome, gotConf, c.outcome, c.conf)
			}
		})
	}
}

// TestScore pins the penalty arithmetic, the caps that stop one noisy dimension
// from sinking the whole score, the grade banding, and the unscored case. The
// numbers are deliberately spelled out so a weight change has to update this table,
// which is the signal that the scoring version must bump alongside it.
func TestScore(t *testing.T) {
	cases := []struct {
		name    string
		signals Signals
		score   int
		grade   string
		scored  bool
	}{
		{
			name:    "clean completed session scores a perfect A",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 10},
			score:   100, grade: "A", scored: true,
		},
		{
			name:    "unknown outcome with no tool signal is unscored",
			signals: Signals{Outcome: OutcomeUnknown},
			score:   0, grade: "", scored: false,
		},
		{
			name:    "unknown outcome with a tool failure is still scored",
			signals: Signals{Outcome: OutcomeUnknown, ToolCalls: 3, ToolFailures: 1},
			score:   97, grade: "A", scored: true,
		},
		{
			name:    "errored outcome takes the errored penalty",
			signals: Signals{Outcome: OutcomeErrored, ToolCalls: 5},
			score:   70, grade: "C", scored: true,
		},
		{
			name:    "abandoned outcome takes the abandoned penalty",
			signals: Signals{Outcome: OutcomeAbandoned, ToolCalls: 5},
			score:   85, grade: "B", scored: true,
		},
		{
			name:    "failures are penalized per call up to the cap",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 20, ToolFailures: 4},
			// 4 * penPerFailure(3) = 12, under capFailures(30)
			score: 88, grade: "B", scored: true,
		},
		{
			name:    "the failure cap bounds a very noisy session",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 50, ToolFailures: 40},
			// 40 * 3 = 120, capped at 30
			score: 70, grade: "C", scored: true,
		},
		{
			name:    "immediate retries are penalized to their cap",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 30, ToolRetries: 10},
			// 10 * penPerRetry(5) = 50, capped at capRetries(25)
			score: 75, grade: "B", scored: true,
		},
		{
			name:    "edit churn is penalized to its cap",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 30, EditChurn: 8},
			// 8 * penPerChurn(4) = 32, capped at capChurn(20)
			score: 80, grade: "B", scored: true,
		},
		{
			name:    "a long failure streak adds a flat penalty",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 30, LongestFailureStreak: 5},
			score:   90, grade: "A", scored: true,
		},
		{
			name:    "a streak below the floor adds nothing",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 30, LongestFailureStreak: 2},
			score:   100, grade: "A", scored: true,
		},
		{
			name: "a thoroughly broken session floors at zero, never negative",
			signals: Signals{
				Outcome: OutcomeErrored, ToolCalls: 100, ToolFailures: 50,
				ToolRetries: 20, EditChurn: 20, LongestFailureStreak: 10,
			},
			// 30 + 30 + 25 + 20 + 10 = 115 penalty, clamped so the score is 0
			score: 0, grade: "F", scored: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score, grade, scored := Score(c.signals)
			if score != c.score || grade != c.grade || scored != c.scored {
				t.Errorf("Score(%+v) = (%d, %q, %v), want (%d, %q, %v)",
					c.signals, score, grade, scored, c.score, c.grade, c.scored)
			}
		})
	}
}

// TestScoreBreakdown cross-checks that the breakdown's arithmetic mirrors Score's: the
// sum of the returned penalty points must equal 100 minus the score (before Score's clamp
// at zero), the ordering must match Score's (outcome, failures, retries, churn, streak),
// and the labels must carry the right count with correct singular/plural. It also pins the
// unscored case (nil) and the clean case (nil, nothing to explain).
func TestScoreBreakdown(t *testing.T) {
	cases := []struct {
		name    string
		signals Signals
		labels  []string
		points  []int
	}{
		{
			name:    "clean completed session has no penalties",
			signals: Signals{Outcome: OutcomeCompleted, ToolCalls: 10},
			labels:  nil, points: nil,
		},
		{
			name:    "unknown outcome with no tool signal is unscored",
			signals: Signals{Outcome: OutcomeUnknown},
			labels:  nil, points: nil,
		},
		{
			name:    "errored ending is the first line",
			signals: Signals{Outcome: OutcomeErrored, ToolCalls: 5},
			labels:  []string{"errored ending"}, points: []int{30},
		},
		{
			name:    "abandoned ending is the first line",
			signals: Signals{Outcome: OutcomeAbandoned, ToolCalls: 5},
			labels:  []string{"abandoned"}, points: []int{15},
		},
		{
			name:    "a single failure is singular",
			signals: Signals{Outcome: OutcomeCompleted, ToolFailures: 1},
			labels:  []string{"1 tool failure"}, points: []int{3},
		},
		{
			name:    "multiple failures are pluralized",
			signals: Signals{Outcome: OutcomeCompleted, ToolFailures: 2},
			labels:  []string{"2 tool failures"}, points: []int{6},
		},
		{
			name:    "a single retry is singular",
			signals: Signals{Outcome: OutcomeCompleted, ToolRetries: 1},
			labels:  []string{"1 retry"}, points: []int{5},
		},
		{
			name:    "multiple retries pluralize retry to retries",
			signals: Signals{Outcome: OutcomeCompleted, ToolRetries: 3},
			labels:  []string{"3 retries"}, points: []int{15},
		},
		{
			name:    "churned edits carry the count",
			signals: Signals{Outcome: OutcomeCompleted, EditChurn: 3},
			labels:  []string{"3 churned edits"}, points: []int{12},
		},
		{
			name:    "a failure streak is a flat line",
			signals: Signals{Outcome: OutcomeCompleted, LongestFailureStreak: 4},
			labels:  []string{"failure streak"}, points: []int{10},
		},
		{
			name: "all lines appear in Score's order",
			signals: Signals{
				Outcome: OutcomeErrored, ToolFailures: 2, ToolRetries: 1,
				EditChurn: 3, LongestFailureStreak: 5,
			},
			labels: []string{"errored ending", "2 tool failures", "1 retry", "3 churned edits", "failure streak"},
			points: []int{30, 6, 5, 12, 10},
		},
		{
			name: "every dimension saturates its cap",
			signals: Signals{
				Outcome: OutcomeErrored, ToolFailures: 40, ToolRetries: 20,
				EditChurn: 20, LongestFailureStreak: 10,
			},
			labels: []string{"errored ending", "40 tool failures", "20 retries", "20 churned edits", "failure streak"},
			points: []int{30, 30, 25, 20, 10},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := ScoreBreakdown(c.signals)
			if len(items) != len(c.labels) {
				t.Fatalf("ScoreBreakdown(%+v) = %d items, want %d: %+v", c.signals, len(items), len(c.labels), items)
			}
			total := 0
			for i, it := range items {
				if it.Label != c.labels[i] {
					t.Errorf("item %d label = %q, want %q", i, it.Label, c.labels[i])
				}
				if it.Points != c.points[i] {
					t.Errorf("item %d points = %d, want %d", i, it.Points, c.points[i])
				}
				if it.Points <= 0 {
					t.Errorf("item %d points = %d, want > 0", i, it.Points)
				}
				total += it.Points
			}
			// The breakdown must reconstruct Score's arithmetic exactly. Score clamps a
			// score below zero to zero, so compare against the pre-clamp penalty (100 minus
			// the reported score) only while the penalties stay under 100; past that the
			// clamp hides the excess and the sum legitimately overshoots 100 minus score.
			score, _, scored := Score(c.signals)
			if !scored {
				if items != nil {
					t.Errorf("unscored session should have nil breakdown, got %+v", items)
				}
				return
			}
			if score > 0 && total != 100-score {
				t.Errorf("breakdown total = %d, want %d (100 - score %d)", total, 100-score, score)
			}
			if score == 0 && total < 100 {
				t.Errorf("breakdown total = %d, want >= 100 for a floored score", total)
			}
		})
	}
}

// TestClassifyArchetype pins the archetype banding: automation wins on no human turn,
// and otherwise the session takes the heaviest band reached by either its duration or
// its message count.
func TestClassifyArchetype(t *testing.T) {
	cases := []struct {
		name  string
		facts ArchetypeFacts
		want  Archetype
	}{
		{"no human turn is automation", ArchetypeFacts{UserMessages: 0, Messages: 500, DurationMin: 300}, ArchetypeAutomation},
		{"tiny exchange is quick", ArchetypeFacts{UserMessages: 1, Messages: 4, DurationMin: 1}, ArchetypeQuick},
		{"reaches standard by messages alone", ArchetypeFacts{UserMessages: 2, Messages: 20, DurationMin: 1}, ArchetypeStandard},
		{"reaches standard by duration alone", ArchetypeFacts{UserMessages: 2, Messages: 6, DurationMin: 10}, ArchetypeStandard},
		{"reaches deep by duration", ArchetypeFacts{UserMessages: 3, Messages: 20, DurationMin: 45}, ArchetypeDeep},
		{"reaches deep by messages", ArchetypeFacts{UserMessages: 3, Messages: 80, DurationMin: 2}, ArchetypeDeep},
		{"marathon by duration", ArchetypeFacts{UserMessages: 4, Messages: 30, DurationMin: 180}, ArchetypeMarathon},
		{"marathon by messages", ArchetypeFacts{UserMessages: 4, Messages: 250, DurationMin: 10}, ArchetypeMarathon},
		{"the heavier of two bands wins", ArchetypeFacts{UserMessages: 2, Messages: 16, DurationMin: 200}, ArchetypeMarathon},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyArchetype(c.facts); got != c.want {
				t.Errorf("ClassifyArchetype(%+v) = %s, want %s", c.facts, got, c.want)
			}
		})
	}
}

// TestGradeForBoundaries pins each banding edge, so a one-point shift in a
// threshold cannot slip through unnoticed.
func TestGradeForBoundaries(t *testing.T) {
	cases := []struct {
		score int
		grade string
	}{
		{100, "A"}, {90, "A"}, {89, "B"}, {75, "B"}, {74, "C"},
		{60, "C"}, {59, "D"}, {40, "D"}, {39, "F"}, {0, "F"},
	}
	for _, c := range cases {
		if got := gradeFor(c.score); got != c.grade {
			t.Errorf("gradeFor(%d) = %q, want %q", c.score, got, c.grade)
		}
	}
}
