package quality

import (
	"hash/fnv"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// shortPromptWords by construction, which is what lets the duplicate aggregate use
	// "not short" as its eligibility test (see PromptFacts): a prompt is either short or
	// duplicate-eligible, never both, so the store needs one stored flag, not two.
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

// maxPleasantryLen is the byte length of the longest pleasantryWords key, the bound isPleasantryWord
// uses to lowercase a candidate token into a stack buffer without a heap allocation, and to reject a
// token too long to be a pleasantry (a pasted-code token) before touching it. A test pins that no
// key exceeds it, so adding a longer greeting word fails loudly rather than silently misclassifying.
const maxPleasantryLen = 9

// PromptFacts are the fixed-size hygiene facts of a single human prompt, derived once when the
// message is written (see ClassifyPrompt). Materializing them at insert is what keeps the whole
// hygiene signal off the settle pass's memory budget: the settle pass aggregates these stored
// columns (booleans and one integer digest per user message) and never reads a prompt body back, so
// its peak memory does not track the largest prompt a session ever held. Computing them at insert
// costs nothing extra: the message body is already in hand there (it is the row being written), so
// this is the one place the text is unavoidably resident.
type PromptFacts struct {
	// Short is whether the prompt fell under shortPromptWords words: a terse steer that carries
	// little intent on its own.
	Short bool
	// NoCodeContext is whether the prompt asked for a code change while pointing at no code (a
	// change verb with no file path, code fence, or inline-code span).
	NoCodeContext bool
	// BareGreeting is whether the prompt is only greeting and pleasantry words. It matters only for
	// the opening turn (an opener that just says hello is an unstructured start), but is computed
	// per prompt so the store can read the opener's flag without fetching its body.
	BareGreeting bool
	// Digest is a normalized fingerprint of the prompt: two prompts equal after lowercasing and
	// collapsing whitespace share a Digest, so the store counts session duplicates as
	// count(*) - count(DISTINCT digest) over the duplicate-eligible (non-short) prompts. It is a
	// 64-bit hash, not the text, so the stored key is fixed size regardless of prompt length.
	Digest int64
}

// ClassifyPrompt derives the fixed-size hygiene facts of one human prompt. It scans the prompt a
// bounded number of times (word count to the terse threshold, the change-verb and anchor regexes,
// the greeting scan, and the streaming digest) and allocates nothing that scales with the prompt
// length, so a pasted-code prompt costs O(len) time and O(1) memory. The store calls it for each
// real human turn (role='user', non-empty content) as the message is inserted and stores the result
// beside the row; the per-turn rules it applies are the same ones the Insights hygiene panel reads.
func ClassifyPrompt(prompt string) PromptFacts {
	return PromptFacts{
		Short:         countWordsUpTo(prompt, duplicateMinWords) < duplicateMinWords,
		NoCodeContext: changeVerbRe.MatchString(prompt) && !hasCodeAnchor(prompt),
		BareGreeting:  isBareGreeting(prompt),
		Digest:        normalizedDigest(prompt),
	}
}

// normalizedDigest hashes prompt after normalizing case and whitespace, streaming the normalized
// bytes into a fixed-size hasher so no normalized copy of the (possibly large) prompt is ever built:
// peak memory is the hasher state, not the prompt length. Normalization lowercases each rune and
// collapses every run of whitespace to a single space with the ends trimmed, so two prompts that
// differ only in case or spacing hash equal and the duplicate signal reads them as one request. The
// hash is FNV-1a, a non-cryptographic fingerprint: it only has to make an accidental collision
// between two genuinely different prompts in one session negligibly unlikely, which 64 bits over a
// session's handful-to-hundreds of prompts easily clears.
func normalizedDigest(prompt string) int64 {
	h := fnv.New64a()
	var buf [utf8.UTFMax]byte
	pendingSpace := false
	started := false
	for _, r := range prompt {
		if unicode.IsSpace(r) {
			if started {
				pendingSpace = true // hold at most one separator, and only between real runes
			}
			continue
		}
		if pendingSpace {
			buf[0] = ' '
			h.Write(buf[:1])
			pendingSpace = false
		}
		started = true
		n := utf8.EncodeRune(buf[:], unicode.ToLower(r))
		h.Write(buf[:n])
	}
	return int64(h.Sum64())
}

// isBareGreeting reports whether every word of the opener is a greeting or pleasantry, so
// an opening turn that only says hello reads as an unstructured start even when it runs
// past the terse word threshold. A single task word takes the opener out of the set.
//
// It scans the tokens in place and short-circuits on the first non-pleasantry word, so a large
// opener (a pasted stack trace, a wall of code) costs only the scan to its first real word and
// never the input-sized lowercased copy and word slice strings.Fields(strings.ToLower(p)) built.
func isBareGreeting(p string) bool {
	seen, allPleasant := false, true
	forEachField(p, func(w string) bool {
		seen = true
		if !isPleasantryWord(w) {
			allPleasant = false
			return false // one real word is enough; stop scanning
		}
		return true
	})
	return seen && allPleasant
}

// greetingPunct is the leading and trailing punctuation a greeting word can carry ("hi," "hello!"),
// stripped before the pleasantry check so the punctuation does not defeat the match.
const greetingPunct = ".,!?;:"

// isPleasantryWord reports whether a token, once its surrounding punctuation is stripped, is one of
// the greeting words. It allocates nothing: it trims and lowercases within a fixed stack buffer and
// relies on the compiler's string([]byte)-as-map-key optimization, so a token of any size (a pasted
// blob) costs O(1) memory. A token longer than the longest pleasantry, or empty after trimming
// (punctuation only), cannot match and is rejected before the buffer is touched.
func isPleasantryWord(w string) bool {
	for len(w) > 0 && strings.IndexByte(greetingPunct, w[0]) >= 0 {
		w = w[1:]
	}
	for len(w) > 0 && strings.IndexByte(greetingPunct, w[len(w)-1]) >= 0 {
		w = w[:len(w)-1]
	}
	if w == "" || len(w) > maxPleasantryLen {
		return false
	}
	var buf [maxPleasantryLen]byte
	for i := 0; i < len(w); i++ {
		c := w[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return pleasantryWords[string(buf[:len(w)])]
}

// countWordsUpTo counts whitespace-separated words in s but stops once it reaches limit, returning
// min(actual, limit). That is exact for any comparison against a threshold at or below limit, and
// unlike strings.Fields it builds no slice, so a large prompt costs O(len) scanning and no
// input-sized allocation.
func countWordsUpTo(s string, limit int) int {
	n := 0
	forEachField(s, func(string) bool {
		n++
		return n < limit
	})
	return n
}

// forEachField calls fn with each whitespace-separated token of s as a substring into s (no
// allocation), matching strings.Fields' splitting (maximal runs of non-space runes, unicode.IsSpace)
// but allocating nothing. It stops early when fn returns false, so a caller that only needs a prefix
// of the tokens pays only for those. This is the shared allocation-free scan the hygiene folder uses
// in place of strings.Fields on prompt text that can be arbitrarily large.
func forEachField(s string, fn func(field string) bool) {
	start := -1
	for i, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				if !fn(s[start:i]) {
					return
				}
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fn(s[start:])
	}
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
