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
	// DuplicateMinWords keeps confirmations out of the duplicate count: two identical
	// "yes" turns are assent, not a re-sent instruction. Only prompts of at least this
	// many words can count as a duplicate, so a duplicate reads as a repeated real
	// request (the agent missed it, or the user gave up waiting and re-sent). It equals
	// shortPromptWords, so a prompt is either short or duplicate-eligible, never both. It
	// is exported because the duplicate count is a database aggregate (see the store's
	// signal gather), so the SQL and this package must share the one threshold.
	DuplicateMinWords = shortPromptWords
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

// PromptHygieneFolder computes a session's per-prompt hygiene signals in one streaming
// pass, holding O(1) state so the store can fold prompts as they arrive from an ordered
// query rather than buffering the whole session. Add the human prompts in order (the first
// Add is the opening turn). The caller passes only real human turns with non-empty content:
// the Claude reducer drops tool-result-only user entries and the store's fetch filters
// empties, so an empty prompt never folds in as a spurious terse turn.
//
// Duplicate detection is deliberately NOT here. Counting exact verbatim repeats needs state
// proportional to the prompt count (a set of everything seen), the whole-session allocation
// this streaming form exists to avoid. The store computes the duplicate count as a database
// aggregate over the same normalized text and threshold (see DuplicateMinWords) and passes
// it to Result, so the memory-bounded per-prompt tests live here and the memory-heavy
// cross-prompt count lives in SQL.
type PromptHygieneFolder struct {
	h     PromptHygiene
	count int
	begun bool
}

// Add folds one human prompt into the running signals.
func (f *PromptHygieneFolder) Add(prompt string) {
	words := len(strings.Fields(prompt))
	if !f.begun {
		// The opening turn sets whether the session started with a clear task.
		f.h.UnstructuredStart = words < shortPromptWords || isBareGreeting(prompt)
		f.begun = true
	}
	if words < shortPromptWords {
		f.h.Short++
	}
	if changeVerbRe.MatchString(prompt) && !hasCodeAnchor(prompt) {
		f.h.NoCodeContext++
	}
	f.count++
}

// Count is the number of prompts folded, the classifier base the store stores so the cohort
// aggregate divides the hygiene counts by exactly the set they came from.
func (f *PromptHygieneFolder) Count() int { return f.count }

// Result returns the folded per-prompt signals with the duplicate count (computed
// separately, see the type doc) filled in.
func (f *PromptHygieneFolder) Result(duplicate int) PromptHygiene {
	h := f.h
	h.Duplicate = duplicate
	return h
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
