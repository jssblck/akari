package quality

import "testing"

// foldHygiene runs prompts through the streaming folder the store uses, so these cases
// exercise the same per-prompt path the ingest signals do. The duplicate count is not a
// per-prompt signal (it is a database aggregate, see PromptHygieneFolder and the store's
// TestSignalsPromptHygiene), so the folder always reports zero for it here; these cases pin
// the per-prompt rules (terse, no-code-context, and the opening turn's structure).
func foldHygiene(prompts []string) (PromptHygiene, int) {
	var f PromptHygieneFolder
	for _, p := range prompts {
		f.Add(p)
	}
	return f.Result(0), f.Count()
}

// TestPromptHygieneFolder pins each per-prompt hygiene rule and the lines it draws. The
// rules are text heuristics, so the cases fix exactly which prompts trip which signal:
// terse turns, change requests with no code anchor, and the opening turn's structure.
func TestPromptHygieneFolder(t *testing.T) {
	cases := []struct {
		name      string
		prompts   []string
		want      PromptHygiene
		wantCount int
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
			prompts:   []string{"what now"},
			want:      PromptHygiene{Short: 1, UnstructuredStart: true},
			wantCount: 1,
		},
		{
			name:      "a substantive opener with a code anchor is clean",
			prompts:   []string{"please refactor the retry loop in internal/server/store/signals.go"},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "confirmations are terse",
			// Four short turns each count short; the opener is short, so the start is
			// unstructured. Duplicate detection is the store's, so it is not tested here.
			prompts:   []string{"yes", "yes", "go on", "continue"},
			want:      PromptHygiene{Short: 4, UnstructuredStart: true},
			wantCount: 4,
		},
		{
			name: "two non-anchored change requests each count no-code-context",
			prompts: []string{
				"add pagination to the sessions list please",
				"add pagination to the sessions list please again",
			},
			// Neither is terse and neither names code while using a change verb, so both
			// count as no-code-context. The opener is not terse, so no unstructured start.
			want:      PromptHygiene{NoCodeContext: 2},
			wantCount: 2,
		},
		{
			name: "a change request with a file anchor is not no-code-context",
			prompts: []string{
				"rewrite the parser entrypoint in parser/claude.go to stream",
			},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "an extensionless config file anchors a change request",
			// Dockerfile carries no extension, so it must match by bare name; otherwise
			// "update the Dockerfile" would be a false no-code-context.
			prompts:   []string{"update the Dockerfile to pin the base image"},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "a dot-prefixed config file anchors a change request",
			// ".gitignore" leads with a dot, which a word-boundary match would miss, so it
			// needs its own anchor rule.
			prompts:   []string{"add build/ to .gitignore"},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "a prose authoring request is not a code change",
			// "write" is not a code-change verb, so a request to author prose with no
			// file reference does not read as no-code-context.
			prompts:   []string{"please write a short overview of this pull request"},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "a change request with a fenced block is anchored",
			prompts: []string{
				"change this to a switch:\n```go\nif x { }\n```",
			},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "a change request naming no code is flagged",
			prompts: []string{
				"can you fix the flaky login test for me",
			},
			want:      PromptHygiene{NoCodeContext: 1},
			wantCount: 1,
		},
		{
			name: "prose with no change verb is never no-code-context",
			prompts: []string{
				"what does this project do and how is it structured",
			},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name:      "a greeting-only opener is an unstructured start even when not terse",
			prompts:   []string{"hello there claude my friend"},
			want:      PromptHygiene{UnstructuredStart: true},
			wantCount: 1,
		},
		{
			name: "a greeting-led opener that carries a real task is not flagged",
			// Opens past the terse threshold and is not a bare greeting (it has a task
			// after the hello), and it names a file, so nothing fires.
			prompts:   []string{"hi, please update the config loader in internal/config/load.go"},
			want:      PromptHygiene{},
			wantCount: 1,
		},
		{
			name: "one slash is not a code anchor, a path of segments is",
			prompts: []string{
				"decide this and/or that and add a helper", // "and/or" is not a path
				"add a helper under internal/server/web",   // a real path anchors it
			},
			// First: change verb ("add"), no anchor -> no-code-context. Second: change
			// verb but a path anchor -> clean. Neither terse; opener not terse.
			want:      PromptHygiene{NoCodeContext: 1},
			wantCount: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, count := foldHygiene(tc.prompts)
			if got != tc.want {
				t.Errorf("foldHygiene(%q) = %+v, want %+v", tc.prompts, got, tc.want)
			}
			if count != tc.wantCount {
				t.Errorf("foldHygiene(%q) count = %d, want %d", tc.prompts, count, tc.wantCount)
			}
		})
	}
}
