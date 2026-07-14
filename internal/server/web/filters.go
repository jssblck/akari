package web

// UnscoredKey is the sentinel a drill-through link and the Grade filter carry for the
// unscored grade bucket, since the empty string reads as "no grade filter".
const UnscoredKey = "unscored"

// IsGrade reports whether v is a grade the session list can filter by: a letter A..F or
// the unscored sentinel. The handler uses it to reject a tampered ?grade= value.
func IsGrade(v string) bool {
	switch v {
	case "A", "B", "C", "D", "F", UnscoredKey:
		return true
	}
	return false
}

// IsOutcome reports whether v is a filterable outcome, so the handler can reject a
// tampered ?outcome= value.
func IsOutcome(v string) bool {
	switch v {
	case "completed", "abandoned", "errored", "unknown":
		return true
	}
	return false
}
