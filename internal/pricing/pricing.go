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

// Version stamps the rate table below. Bump it whenever a rate in `table` changes: a new or
// removed model, or a different Input/Output/CacheWrite/CacheRead number for an existing one.
//
// It exists because a reprice makes two stored figures go stale in different ways, and only one
// of them is fixed by the reparse that a reprice already triggers. Per-row cost is stored on each
// usage_events row at parse time, so a reprice reparses the corpus (via parse.Epoch) to rewrite
// every row's cost; a session that fails to reparse keeps old cost, which is the honest state for
// a transcript the parser can no longer rebuild. The per-session cache-savings rollup is
// different: it is priced from usage_events, not stored per row, and a failed-reparse session
// keeps its old-priced rollup with cache_savings_backfilled=true, so nothing re-prices it and its
// tile drifts from a live SessionCacheStats recompute forever. The cache-savings reconcile
// (store.reconcileCacheSavingsPricingIfNeeded) closes that gap by re-pricing every cache-bearing
// session on a Version change, independent of whether its reparse succeeds.
//
// Version is deliberately separate from parse.Epoch. Epoch bumps for any parser or reducer change
// (a new projection column, a changed field), most of which do not touch rates; keying the
// cache-savings reconcile off Epoch would re-price the whole corpus on every unrelated Epoch bump.
// A dedicated pricing Version fires that reconcile only on an actual rate change. Pair a Version
// bump with a parse.Epoch bump, as any reprice already must, so per-row cost and the cache-savings
// rollup are both rebuilt on the same deploy.
//
// Version 1 -> 2: add claude-sonnet-5 (Sonnet 5 at the standard $3/$15 Sonnet rate). Sonnet 5
// priced as unknown before, so a cache-bearing Sonnet 5 session carried an unpriced cache-savings
// rollup; this bump fires reconcileCacheSavingsPricingIfNeeded to re-price the cache-bearing corpus,
// paired with the parse.Epoch 9 -> 10 reparse that rewrites each Sonnet 5 usage row's per-row cost.
const Version = 2

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

	// Sonnet: $3/$15 from 3.5 through 5. Sonnet 5 keeps the $3/$15 sticker; its
	// introductory $2/$10 promo (through 2026-08-31) is deliberately not encoded,
	// since the table carries one durable rate per model, not a time-windowed one.
	// "claude-sonnet-4" is Sonnet 4.0's dateless ID (claude-sonnet-4-20250514
	// normalizes to it).
	"claude-sonnet-5":   {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
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

// CacheSavings returns the USD that prompt caching saved versus paying the full
// uncached input rate for the same prompt tokens, and whether the model was priced.
//
// Caching changes only the prompt side. A token served from cache (cacheRead) would
// otherwise be billed at the input rate; a token written to cache (cacheWrite) would
// otherwise be a plain input token too. So the saving is the rate gap on each, summed:
// cacheRead*(Input-CacheRead) + cacheWrite*(Input-CacheWrite).
//
// For Claude the cacheWrite term is negative: cache creation is priced above input
// (the premium paid up front to make later reads cheap), so netting it in keeps the
// figure honest rather than advertising only the read discount. For OpenAI the Codex
// parser reports cache creation as ordinary input (CacheWrite is unset and cacheWrite
// tokens are nil), so the write term vanishes and the saving is the read discount
// alone. The result can be negative in principle (cache written but never re-read) and
// is returned unfloored, so a caller can surface that caching cost more than it saved.
//
// Counts are int64, not the int that Cost takes: this is the one pricing entry point
// fed rolled, fleet-wide aggregates, whose cache-read sum over a long window can run
// past a 32-bit range, where Cost only ever sees a single session's tokens.
func CacheSavings(model string, cacheRead, cacheWrite int64) (float64, bool) {
	r, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	saving := float64(cacheRead)/million*(r.Input-r.CacheRead) +
		float64(cacheWrite)/million*(r.Input-r.CacheWrite)
	return saving, true
}
