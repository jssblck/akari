package quality

import "testing"

// TestClassify pins the outcome rule and the precedence between its cases. The
// ordering matters: a session that ends badly must not read "completed" merely
// because the assistant happened to speak last, so the strong signals (no human,
// mid-tool, an errored tail) are checked before the last-word heuristic.
func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		facts   Facts
		outcome Outcome
		conf    Confidence
	}{
		{
			name:    "no human turn is unknown",
			facts:   Facts{UserMessages: 0, LastAssistantOrd: 5, LastUserOrd: -1},
			outcome: OutcomeUnknown, conf: ConfLow,
		},
		{
			name:    "pending tool call holds the verdict even past the last word",
			facts:   Facts{UserMessages: 2, LastAssistantOrd: 9, LastUserOrd: 4, ToolCallPending: true},
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
