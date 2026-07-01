package quality

import (
	"strings"
	"testing"
)

// TestClassifyPrompt pins each per-prompt hygiene rule and the lines it draws. The rules are
// text heuristics, so the cases fix exactly which prompt trips which flag: terse steers, change
// requests with no code anchor, and bare greetings. The whole-session aggregation (the short and
// no-code counts, the duplicate count, and the opener's unstructured-start verdict) is the store's,
// so it is pinned end to end in the store signal tests, not here.
func TestClassifyPrompt(t *testing.T) {
	cases := []struct {
		name     string
		prompt   string
		short    bool
		noCode   bool
		greeting bool
	}{
		{"a terse steer is short", "what now", true, false, false},
		{"a bare confirmation is short", "yes", true, false, false},
		{"a substantive anchored request trips nothing", "please refactor the retry loop in internal/server/store/signals.go", false, false, false},
		{"a non-anchored change request is no-code-context", "add pagination to the sessions list please", false, true, false},
		{"a change request with a file anchor is anchored", "rewrite the parser entrypoint in parser/claude.go to stream", false, false, false},
		// Dockerfile carries no extension, so it must match by bare name; otherwise the change
		// request naming it would be a false no-code-context.
		{"an extensionless config file anchors a change request", "update the Dockerfile to pin the base image", false, false, false},
		// ".gitignore" leads with a dot, which a word-boundary match would miss, so it needs its
		// own anchor rule.
		{"a dot-prefixed config file anchors a change request", "add build/ to .gitignore", false, false, false},
		// "write" is not a code-change verb, so authoring prose with no file reference is not
		// no-code-context.
		{"a prose authoring request is not a code change", "please write a short overview of this pull request", false, false, false},
		{"a fenced block anchors a change request", "change this to a switch:\n```go\nif x { }\n```", false, false, false},
		{"a change request naming no code is flagged", "can you fix the flaky login test for me", false, true, false},
		{"prose with no change verb is never no-code-context", "what does this project do and how is it structured", false, false, false},
		{"a greeting-only line is a bare greeting", "hello there claude my friend", false, false, true},
		// Opens past the terse threshold and is not a bare greeting (a task follows the hello),
		// and it names a file, so nothing fires.
		{"a greeting-led line carrying a task is not a bare greeting", "hi, please update the config loader in internal/config/load.go", false, false, false},
		// "and/or" is one slash, not a path, so the change verb with no anchor is no-code-context.
		{"one slash is not a code anchor", "decide this and/or that and add a helper", false, true, false},
		// A real multi-segment path anchors the change request.
		{"a path of segments is a code anchor", "add a helper under internal/server/web", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyPrompt(tc.prompt)
			if got.Short != tc.short || got.NoCodeContext != tc.noCode || got.BareGreeting != tc.greeting {
				t.Errorf("ClassifyPrompt(%q) = {short %v, noCode %v, greeting %v}, want {%v, %v, %v}",
					tc.prompt, got.Short, got.NoCodeContext, got.BareGreeting, tc.short, tc.noCode, tc.greeting)
			}
		})
	}
}

// TestNormalizedDigest pins the fingerprint the store's duplicate count groups on: prompts that
// differ only in case or whitespace must hash equal (they are the same request re-sent), and
// genuinely different prompts must not. The digest is what lets the duplicate count be a fixed-size
// aggregate instead of a re-normalization of every prompt body.
func TestNormalizedDigest(t *testing.T) {
	if normalizedDigest("Explain the retry logic here") != normalizedDigest("explain   the   retry logic here") {
		t.Error("case and whitespace variants of one prompt should hash equal")
	}
	if normalizedDigest("  hello world  ") != normalizedDigest("hello world") {
		t.Error("surrounding whitespace should be trimmed before hashing")
	}
	if normalizedDigest("") != normalizedDigest("  \t\n ") {
		t.Error("a whitespace-only prompt should normalize to empty")
	}
	if normalizedDigest("review the pagination approach") == normalizedDigest("review the caching approach") {
		t.Error("genuinely different prompts should not share a digest")
	}
}

// TestPleasantryWordsFitBuffer pins the invariant isPleasantryWord's fixed stack buffer relies on:
// no greeting word is longer than maxPleasantryLen. A longer word added to the set would hit the
// length reject and be silently misclassified as non-pleasant, so catch it here rather than in
// production classification.
func TestPleasantryWordsFitBuffer(t *testing.T) {
	for w := range pleasantryWords {
		if len(w) > maxPleasantryLen {
			t.Errorf("pleasantry word %q is %d bytes, over maxPleasantryLen %d; raise the constant", w, len(w), maxPleasantryLen)
		}
	}
}

// TestHygieneHandlesLargePrompt pins that the allocation-free scans classify a very large prompt the
// same as a small one and cost only bounded memory. This is the case the per-message rewrite exists
// for: reading and folding prompt bodies at settle time would have made peak memory track the largest
// pasted-code prompt a session ever held, so the facts are derived once at insert with scans that
// build no input-sized slice or normalized copy.
func TestHygieneHandlesLargePrompt(t *testing.T) {
	big := strings.Repeat("token ", 50_000) // ~300KB of words, far past every threshold
	// A change verb, a real file anchor, and a wall of words: anchored (not no-code-context) and
	// not terse, exactly as the same request would read at small size.
	if got := ClassifyPrompt("refactor internal/server/store/read.go " + big); got.Short || got.NoCodeContext || got.BareGreeting {
		t.Errorf("large anchored change request = %+v, want no per-prompt flags", got)
	}
	// The streaming digest is deterministic and never builds a normalized copy of the body.
	if normalizedDigest(big) != normalizedDigest(big) {
		t.Error("the digest should be deterministic for one input")
	}
	// A huge opener of only pleasantries is still a bare greeting; the scan runs to the end without
	// short-circuiting and matches every token.
	if !isBareGreeting("hello there " + strings.Repeat("please ", 20_000)) {
		t.Error("a large all-pleasantry opener should still read as a bare greeting")
	}
	// One real word ends it, and the scan short-circuits at that first token rather than reading the
	// wall behind it.
	if isBareGreeting("hello there " + big) {
		t.Error("a large opener carrying a non-greeting word is not a bare greeting")
	}
}
