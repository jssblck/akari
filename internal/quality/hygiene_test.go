package quality

import "testing"

// TestClassifyPromptHygiene pins each hygiene rule and the lines it draws. The rules are
// text heuristics, so the cases fix exactly which prompts trip which signal: terse turns,
// verbatim repeats among real requests (not confirmations), change requests with no code
// anchor, and the opening turn's structure.
func TestClassifyPromptHygiene(t *testing.T) {
	cases := []struct {
		name    string
		prompts []string
		want    PromptHygiene
	}{
		{
			name:    "empty session has no signal",
			prompts: nil,
			want:    PromptHygiene{},
		},
		{
			name: "a terse opener is short and an unstructured start",
			// No change verb, so only the terse and unstructured-start signals fire; a
			// terse "fix it" would additionally count as no-code-context.
			prompts: []string{"what now"},
			want:    PromptHygiene{Short: 1, UnstructuredStart: true},
		},
		{
			name:    "a substantive opener with a code anchor is clean",
			prompts: []string{"please refactor the retry loop in internal/server/store/signals.go"},
			want:    PromptHygiene{},
		},
		{
			name: "confirmations are terse but never duplicates",
			// Four short turns: each counts short, none counts duplicate (too short to be
			// a re-sent instruction). The opener is short, so the start is unstructured.
			prompts: []string{"yes", "yes", "go on", "continue"},
			want:    PromptHygiene{Short: 4, UnstructuredStart: true},
		},
		{
			name: "a repeated real request counts once, on the repeat",
			prompts: []string{
				"add pagination to the sessions list please",
				"add  Pagination to the sessions LIST please", // same after normalizing case and spaces
			},
			// Neither is terse (both are long requests). The first seeds the set, the
			// second is the duplicate. Neither names code, and both use a change verb, so
			// both also count as no-code-context. The opener is not terse, so no
			// unstructured start.
			want: PromptHygiene{Duplicate: 1, NoCodeContext: 2},
		},
		{
			name: "a change request with a file anchor is not no-code-context",
			prompts: []string{
				"rewrite the parser entrypoint in parser/claude.go to stream",
			},
			want: PromptHygiene{},
		},
		{
			name: "an extensionless config file anchors a change request",
			// Dockerfile carries no extension, so it must match by bare name; otherwise
			// "update the Dockerfile" would be a false no-code-context.
			prompts: []string{"update the Dockerfile to pin the base image"},
			want:    PromptHygiene{},
		},
		{
			name: "a dot-prefixed config file anchors a change request",
			// ".gitignore" leads with a dot, which a word-boundary match would miss, so it
			// needs its own anchor rule.
			prompts: []string{"add build/ to .gitignore"},
			want:    PromptHygiene{},
		},
		{
			name: "a prose authoring request is not a code change",
			// "write" is not a code-change verb, so a request to author prose with no
			// file reference does not read as no-code-context.
			prompts: []string{"please write a short overview of this pull request"},
			want:    PromptHygiene{},
		},
		{
			name: "a change request with a fenced block is anchored",
			prompts: []string{
				"change this to a switch:\n```go\nif x { }\n```",
			},
			want: PromptHygiene{},
		},
		{
			name: "a change request naming no code is flagged",
			prompts: []string{
				"can you fix the flaky login test for me",
			},
			want: PromptHygiene{NoCodeContext: 1},
		},
		{
			name: "prose with no change verb is never no-code-context",
			prompts: []string{
				"what does this project do and how is it structured",
			},
			want: PromptHygiene{},
		},
		{
			name:    "a greeting-only opener is an unstructured start even when not terse",
			prompts: []string{"hello there claude my friend"},
			want:    PromptHygiene{UnstructuredStart: true},
		},
		{
			name: "a greeting-led opener that carries a real task is not flagged",
			// Opens past the terse threshold and is not a bare greeting (it has a task
			// after the hello), and it names a file, so nothing fires.
			prompts: []string{"hi, please update the config loader in internal/config/load.go"},
			want:    PromptHygiene{},
		},
		{
			name: "one slash is not a code anchor, a path of segments is",
			prompts: []string{
				"decide this and/or that and add a helper", // "and/or" is not a path
				"add a helper under internal/server/web",   // a real path anchors it
			},
			// First: change verb ("add"), no anchor -> no-code-context. Second: change
			// verb but a path anchor -> clean. Neither terse; opener not terse.
			want: PromptHygiene{NoCodeContext: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyPromptHygiene(tc.prompts)
			if got != tc.want {
				t.Errorf("ClassifyPromptHygiene(%q) = %+v, want %+v", tc.prompts, got, tc.want)
			}
		})
	}
}

// TestClassifyPromptHygieneDuplicateThreshold confirms the duplicate rule only fires among
// prompts long enough to be a real request: a repeated terse steer is assent, not a
// re-sent instruction, so it stays out of the duplicate count.
func TestClassifyPromptHygieneDuplicateThreshold(t *testing.T) {
	// Two identical four-word requests: four words clears the terse threshold, so the
	// second is a duplicate. The change verb with no anchor flags both as no-code-context.
	got := ClassifyPromptHygiene([]string{"add the missing test", "add the missing test"})
	if got.Duplicate != 1 {
		t.Errorf("duplicate = %d, want 1 for a repeated four-word request", got.Duplicate)
	}
	if got.Short != 0 {
		t.Errorf("short = %d, want 0 (four words is not terse)", got.Short)
	}
}
