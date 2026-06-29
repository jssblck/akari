package parse

import (
	"errors"
	"fmt"
	"testing"
)

// TestParserErrorClassification confirms the reparse service can tell a deterministic
// parser failure apart from an operational one even after the error is wrapped with
// region context, which is what lets it tolerate the former (count and continue, the
// epoch still advances) and abort on the latter (retry, the epoch is left behind).
func TestParserErrorClassification(t *testing.T) {
	cause := errors.New("malformed transcript line")
	// Wrapped the way ReparseSession wraps a reducer failure: with region context.
	wrapped := fmt.Errorf("parse session 7 region [0,20): %w", &ParserError{err: cause})

	var pe *ParserError
	if !errors.As(wrapped, &pe) {
		t.Fatal("a wrapped ParserError should be recoverable with errors.As")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("a ParserError should unwrap to its underlying cause")
	}

	// An operational error (a store query or CAS failure) must not classify as a parser
	// error, so the service aborts the run instead of stamping the epoch over it.
	operational := fmt.Errorf("reparse session 7: %w", errors.New("connection reset by peer"))
	if errors.As(operational, &pe) {
		t.Fatal("an operational error must not classify as a ParserError")
	}
}
