package quality

import (
	"regexp"
	"strings"
)

// Prompt-hygiene thresholds. They are part of the signals version (bump Version on any
// change), so a stored count records which rule produced it. The rules below are
// deliberately narrow: each hygiene signal flags a real input problem, and a false
// positive on a "problem" flag is worse than a miss, so where a rule could go either way
// it errs toward not flagging.
const (
	// shortPromptWords is the word count below which a human prompt reads as terse. A
	// one- or two-word steer ("yes", "keep going", "fix it") carries little intent on its
	// own. A short follow-up inside a rich thread is often fine, so this is a hygiene
	// signal rather than a fault, but a session driven mostly by terse prompts is worth
	// surfacing.
	shortPromptWords = 4
	// duplicateMinWords keeps confirmations out of the duplicate count: two identical
	// "yes" turns are assent, not a re-sent instruction. Only prompts of at least this
	// many words can count as a duplicate, so a duplicate reads as a repeated real
	// request (the agent missed it, or the user gave up waiting and re-sent). It equals
	// shortPromptWords, so a prompt is either short or duplicate-eligible, never both.
	duplicateMinWords = shortPromptWords
)

// PromptHygiene is a session's input-quality summary, computed from its ordered human
// prompts. The counts feed the Insights prompt-hygiene panel and the session signals
// strip. They are pure functions of the prompt text, so they live here beside the scoring
// model and are unit tested without a database, and they do NOT feed Score: an unclear
// prompt is the human's to fix, not a mark against the agent.
type PromptHygiene struct {
	// Short is how many prompts fell under shortPromptWords words.
	Short int
	// Duplicate is how many prompts repeated an earlier prompt verbatim (after
	// normalizing case and whitespace), counting each repeat beyond the first and only
	// among prompts long enough to be a real request (see duplicateMinWords).
	Duplicate int
	// NoCodeContext is how many prompts asked for a code change while pointing at no
	// code: no file path, no code fence, no inline-code span. Naming what to change
	// without showing where makes the agent guess.
	NoCodeContext int
	// UnstructuredStart reports whether the opening prompt was terse or a bare greeting,
	// so the session began without a clear task set.
	UnstructuredStart bool
}

var (
	// changeVerbRe matches an imperative code-change verb as a whole word. A prompt that
	// contains one is asking for a change; paired with the absence of a code anchor it
	// marks a no-code-context request. The set is kept to verbs that read as code edits in
	// a coding-agent thread. Broad authoring verbs (write, create, build) are deliberately
	// out: "write a PR description" or "build a release note" are prose, not code, and
	// counting them would flag ordinary prompts as no-code-context.
	changeVerbRe = regexp.MustCompile(`(?i)\b(fix|change|add|implement|refactor|update|remove|delete|rename|replace|edit|modify|patch|revert|migrate|rewrite|extend|adjust|tweak)\b`)
	// fileExtRe matches a filename with a source-code extension (foo.go, App.tsx). A
	// named file is a concrete code anchor.
	fileExtRe = regexp.MustCompile(`(?i)\b[\w.-]+\.(go|ts|tsx|js|jsx|mjs|cjs|py|rs|rb|java|kt|kts|c|cc|cpp|cxx|h|hpp|hxx|cs|php|swift|scala|clj|ex|exs|sh|bash|zsh|sql|css|scss|sass|less|html|htm|xml|ya?ml|toml|ini|cfg|md|rst|txt|proto|graphql|vue|svelte)\b`)
	// bareFileRe matches the common code and config files that carry no extension (or an
	// unusual one) and start with a word character, so "update Dockerfile" or "the go.mod"
	// reads as anchored the same way a dotted filename does. Without this those names would
	// slip past fileExtRe and a change request naming one would be a false no-code-context.
	bareFileRe = regexp.MustCompile(`(?i)\b(dockerfile|makefile|rakefile|gemfile|procfile|jenkinsfile|vagrantfile|justfile|go\.mod|go\.sum|cargo\.lock)\b`)
	// dotFileRe matches dot-prefixed config files. It cannot lean on a leading \b: the dot
	// is a non-word character, so between a preceding space and the dot there is no
	// word/non-word transition and \b would never fire. Anchor on start-of-string or a
	// preceding non-word, non-dot character instead, so ".env" reads as an anchor but
	// "foo.env" (a dotted extension fileExtRe already covers) is left to that rule.
	dotFileRe = regexp.MustCompile(`(?i)(^|[^\w.])\.(gitignore|dockerignore|env)\b`)
	// pathRe matches a slash-separated directory path of at least two segments past the
	// first (internal/server/store). One slash alone ("and/or") is not a path; two or
	// more read as one.
	pathRe = regexp.MustCompile(`[\w.-]+/[\w.-]+/[\w./-]+`)
)

// pleasantryWords is the vocabulary of a bare greeting: an opener built only from these
// (hello, small talk, a name) sets no task. One real word (a verb, a noun, a filename)
// takes the opener out of the set, so "hi, fix auth.go" is not a bare greeting while
// "hey there, how are you doing today" is.
var pleasantryWords = map[string]bool{
	"hi": true, "hey": true, "hello": true, "yo": true, "sup": true, "howdy": true,
	"greetings": true, "good": true, "morning": true, "afternoon": true, "evening": true,
	"there": true, "all": true, "everyone": true, "team": true, "folks": true,
	"claude": true, "friend": true, "my": true, "how": true, "are": true, "you": true,
	"doing": true, "today": true, "please": true, "thanks": true, "hiya": true,
}

// ClassifyPromptHygiene computes a session's hygiene counts from its human prompts in
// order (prompts[0] is the opening turn). The caller passes only real human turns with
// non-empty content: the Claude reducer already drops tool-result-only user entries, and
// the store's fetch filters empties, so an empty prompt never reaches here to read as a
// spurious terse turn.
func ClassifyPromptHygiene(prompts []string) PromptHygiene {
	var h PromptHygiene
	// seen remembers the normalized text of each non-terse prompt so a later verbatim
	// repeat counts as a duplicate. Exact within-session duplicate detection is inherently
	// stateful: a prompt cannot be known to repeat without remembering the ones before it.
	// The map is bounded by this session's prompt count (human turns are few and each key
	// is one normalized message body, not a tool payload) and is released when the function
	// returns, so it holds kilobytes for a real session. A fixed recent window would cap it
	// further but would miss a repeat of an older prompt, a trade a bounded session does
	// not need.
	seen := make(map[string]bool, len(prompts))
	for i, p := range prompts {
		words := len(strings.Fields(p))
		switch {
		case words < shortPromptWords:
			h.Short++
		default:
			// Only non-terse prompts can be duplicates, so repeated assent does not read
			// as a re-sent instruction. The first occurrence seeds the set; a later match
			// on the normalized text counts.
			norm := normalizePrompt(p)
			if seen[norm] {
				h.Duplicate++
			} else {
				seen[norm] = true
			}
		}
		if changeVerbRe.MatchString(p) && !hasCodeAnchor(p) {
			h.NoCodeContext++
		}
		if i == 0 {
			h.UnstructuredStart = words < shortPromptWords || isBareGreeting(p)
		}
	}
	return h
}

// normalizePrompt collapses a prompt to a canonical form for duplicate detection:
// lowercased, with runs of whitespace folded to single spaces and the ends trimmed. Two
// prompts that differ only in case or spacing normalize equal.
func normalizePrompt(p string) string {
	return strings.Join(strings.Fields(strings.ToLower(p)), " ")
}

// isBareGreeting reports whether every word of the opener is a greeting or pleasantry, so
// an opening turn that only says hello reads as an unstructured start even when it runs
// past the terse word threshold. A single task word takes the opener out of the set.
func isBareGreeting(p string) bool {
	fields := strings.Fields(strings.ToLower(p))
	if len(fields) == 0 {
		return false
	}
	for _, w := range fields {
		if !pleasantryWords[strings.Trim(w, ".,!?;:")] {
			return false
		}
	}
	return true
}

// hasCodeAnchor reports whether a prompt points at concrete code: a fenced block, an
// inline-code span, a named source file, or a multi-segment path. These are the anchors
// that let the agent act without guessing; their absence in a change request is what
// NoCodeContext flags.
func hasCodeAnchor(p string) bool {
	switch {
	case strings.Contains(p, "```"):
		return true
	case strings.Count(p, "`") >= 2:
		return true
	case fileExtRe.MatchString(p):
		return true
	case bareFileRe.MatchString(p):
		return true
	case dotFileRe.MatchString(p):
		return true
	case pathRe.MatchString(p):
		return true
	default:
		return false
	}
}
