package parse

import (
	"errors"
	"fmt"
	"testing"
)

// TestParserErrorClassification confirms the worker can tell a deterministic
// parser failure apart from an operational one even after the error is wrapped
// with region context, which is what lets it count the former as done (the
// store recorded the attempt on the failure markers) and leave the latter due
// for the next drain to retry.
func TestParserErrorClassification(t *testing.T) {
	cause := errors.New("malformed transcript line")
	// Wrapped the way RebuildSession wraps a reducer failure: with region context.
	wrapped := fmt.Errorf("parse session 7 region [0,20): %w", &ParserError{err: cause})

	var pe *ParserError
	if !errors.As(wrapped, &pe) {
		t.Fatal("a wrapped ParserError should be recoverable with errors.As")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("a ParserError should unwrap to its underlying cause")
	}

	// An operational error (a store query or CAS failure) must not classify as a
	// parser error, so the session stays due and the next drain retries it.
	operational := fmt.Errorf("rebuild session 7: %w", errors.New("connection reset by peer"))
	if errors.As(operational, &pe) {
		t.Fatal("an operational error must not classify as a ParserError")
	}
	if !isParserError(wrapped) || isParserError(operational) {
		t.Fatal("isParserError must agree with the errors.As classification")
	}
}
