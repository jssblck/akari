// Package pricing computes session cost from a model rate table compiled into
// the binary. There is no runtime catalog or refresh: updating rates means a new
// build. Rates are a snapshot in USD per one million tokens and are intentionally
// approximate; an unknown model yields known=false so callers can mark a cost as
// partial rather than reporting a misleading zero.
package pricing

import (
	"regexp"
	"strings"
)

// Rate holds per-million-token prices for one model family.
type Rate struct {
	Input      float64
	Output     float64
	CacheWrite float64 // cache creation
	CacheRead  float64
}

// table maps a canonical model ID to its rate. Matching is EXACT, not by prefix:
// a key prices only the model whose ID it is, never a whole family or major line.
// That is deliberate. Pricing has diverged within a major line before (Opus 4.1
// at $15/$75 then Opus 4.5 at $5/$25; GPT-5 at $1.25/$10 then GPT-5.5 at $5/$30),
// so a prefix that looked uniform today would silently misprice the next version
// that repriced. With exact matching that whole bug class is impossible: a model
// we have not listed (a new minor, a new variant) falls through to known=false
// and is reported as an incomplete cost rather than a wrong number.
//
// Keys are the canonical, dateless IDs. Lookup strips a trailing release-date
// snapshot before matching (see datedSnapshot), so both the alias
// (claude-opus-4-8) and the dated ID (claude-opus-4-8-20260115) resolve to one
// key. A model whose dateless ID carries no minor number (Opus 4.0's
// claude-opus-4-20250514 normalizes to "claude-opus-4") is keyed by that bare
// ID; under exact matching that is the model's own name, not a catch-all.
//
// TestUnlistedModelsAreUnknown guards that future and sibling models stay
// unknown. When adding a model, add its exact ID; do not widen an existing key.
var table = map[string]Rate{
	// Fable 5 and Mythos 5 share pricing; mythos-preview is the invitation-only
	// predecessor at the same rate.
	"claude-fable-5":        {Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00},
	"claude-mythos-5":       {Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00},
	"claude-mythos-preview": {Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00},

	// Opus: 4.0/4.1 at $15/$75, 4.5 onward at $5/$25. "claude-opus-4" is Opus
	// 4.0's dateless ID (claude-opus-4-20250514 normalizes to it).
	"claude-opus-4":   {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-opus-4-0": {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-opus-4-1": {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-opus-4-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-6": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-7": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-8": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},

	// Sonnet: $3/$15 from 3.5 through 4.6. "claude-sonnet-4" is Sonnet 4.0's
	// dateless ID (claude-sonnet-4-20250514 normalizes to it).
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-sonnet-4-0": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-sonnet-4-5": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-3-7-sonnet": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-3-5-sonnet": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},

	// Haiku: 4.5 is the only 4.x; 3.5 is the prior generation.
	"claude-haiku-4-5": {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
	"claude-3-5-haiku": {Input: 0.80, Output: 4, CacheWrite: 1, CacheRead: 0.08},

	// OpenAI GPT-5 family, current generation (June 2026).
	//
	// CacheWrite is deliberately left unset (zero) for every OpenAI model, and that
	// is not a missing rate: OpenAI does not bill cache creation as its own line.
	// Caching there is automatic and free to write, so a token newly cached is
	// charged once at the standard input rate, and only re-reads of it are
	// discounted (CacheRead). The Codex parser reflects this by reporting the whole
	// uncached remainder (total prompt minus cached) as Input and only the cached
	// hits as CacheRead, so those cache-write tokens are already priced at Input.
	// Adding a nonzero CacheWrite here would double-count them: OpenAI never reports
	// a separate cache-write count for it to multiply, so it must stay zero.
	//
	// The -pro tiers carry no CacheRead on purpose: OpenAI disables prompt-cache
	// retention for them, so a repeated prefix is re-billed at full input ($30/M)
	// with no discounted cached read to price. Their cached-input column on the
	// pricing page reads "not available", not a number. Leave CacheRead unset; a
	// cached read never reaches a -pro model.
	"gpt-5.5":       {Input: 5, Output: 30, CacheRead: 0.50},
	"gpt-5.5-pro":   {Input: 30, Output: 180},
	"gpt-5.4":       {Input: 2.50, Output: 15, CacheRead: 0.25},
	"gpt-5.4-mini":  {Input: 0.75, Output: 4.50, CacheRead: 0.075},
	"gpt-5.4-nano":  {Input: 0.20, Output: 1.25, CacheRead: 0.02},
	"gpt-5.4-pro":   {Input: 30, Output: 180},
	"gpt-5.3-codex": {Input: 1.75, Output: 14, CacheRead: 0.175},
	// Prior generation (GPT-5, Aug 2025 launch). gpt-5-2025-08-07 normalizes to
	// "gpt-5"; under exact matching that base key does not absorb gpt-5.4/gpt-5.5.
	"gpt-5":       {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-codex": {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-mini":  {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5-nano":  {Input: 0.05, Output: 0.40, CacheRead: 0.005},
}

// datedSnapshot matches a trailing release-date suffix in either the Anthropic
// form (-20250514) or the OpenAI form (-2025-08-07). Stripping it maps a dated
// model ID back to its canonical key so both forms price identically.
var datedSnapshot = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

// Lookup returns the rate for a model and whether it was found. The model string
// is normalized (lowercased, trimmed, and stripped of a trailing release-date
// snapshot) and then matched exactly against the table. There is no prefix
// matching: a key prices only its exact model, so a model we have not listed
// reports known=false rather than inheriting a neighbor's price.
func Lookup(model string) (Rate, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	model = datedSnapshot.ReplaceAllString(model, "")
	if model == "" {
		return Rate{}, false
	}
	r, ok := table[model]
	return r, ok
}

// Cost returns the USD cost for a token count under a model, and whether the
// model was priced. Token counts are in tokens (not millions).
func Cost(model string, input, output, cacheWrite, cacheRead int) (float64, bool) {
	r, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	cost := float64(input)/million*r.Input +
		float64(output)/million*r.Output +
		float64(cacheWrite)/million*r.CacheWrite +
		float64(cacheRead)/million*r.CacheRead
	return cost, true
}
