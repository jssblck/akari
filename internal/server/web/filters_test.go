package web

import "testing"

// IsGrade is the whitelist behind the session feed's ?grade= param: the letter
// grades, the unscored sentinel, and nothing else, so a tampered value is a 400
// rather than a silent fall-through to the unfiltered list.
func TestIsGrade(t *testing.T) {
	for _, ok := range []string{"A", "B", "C", "D", "F", UnscoredKey} {
		if !IsGrade(ok) {
			t.Errorf("IsGrade(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a", "E", "AA", "unknown", "Unscored"} {
		if IsGrade(bad) {
			t.Errorf("IsGrade(%q) = true, want false", bad)
		}
	}
}

// IsOutcome mirrors IsGrade for the ?outcome= param: exactly the stored outcome
// values pass, including the explicit "unknown" bucket.
func TestIsOutcome(t *testing.T) {
	for _, ok := range []string{"completed", "abandoned", "errored", "unknown"} {
		if !IsOutcome(ok) {
			t.Errorf("IsOutcome(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Completed", "failed", "done"} {
		if IsOutcome(bad) {
			t.Errorf("IsOutcome(%q) = true, want false", bad)
		}
	}
}
