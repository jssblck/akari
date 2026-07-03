// Package pricing computes session cost from a model rate table compiled into
// the binary. There is no runtime catalog or refresh: updating rates means a new
// build. Rates are a snapshot in USD per one million tokens and are intentionally
// approximate; an unknown model yields known=false so callers can mark a cost as
// partial rather than reporting a misleading zero.
//
// A model's price carries a time dimension: each model maps to a list of
// date-effective rates, and a lookup selects the entry in effect at the usage
// event's time. That lets one model ID price pre-change and post-change usage
// differently (an introductory promo that reverts on a date, or a mid-life
// reprice) without inventing a second ID. A single-entry list is the common case
// and reproduces a flat rate: the one window is in effect for all time.
package pricing

import (
	"regexp"
	"strings"
	"time"
)

// Version stamps the rate table below. Bump it whenever a rate in `table` changes: a new or
// removed model, a different Input/Output/CacheWrite/CacheRead number for an existing one, or a
// new or moved date-effective window.
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
//
// Version 2 -> 3: give claude-sonnet-5 its two-window introductory rate ($2/$10 per MTok through
// 2026-08-31, reverting to the $3/$15 sticker on 2026-09-01), replacing the single flat $3/$15
// entry Version 2 encoded. A Sonnet 5 event logged inside the intro window now prices cheaper, so
// this is a reprice: it fires reconcileCacheSavingsPricingIfNeeded to re-price the cache-bearing
// corpus at the windowed rates, paired with the parse.Epoch 10 -> 11 reparse that rewrites each
// Sonnet 5 usage row's per-row cost from the window in effect at its OccurredAt.
const Version = 3

// Rate holds per-million-token prices for one model family.
type Rate struct {
	Input      float64
	Output     float64
	CacheWrite float64 // cache creation
	CacheRead  float64
}

// DatedRate is a rate that took effect on a date and stays in effect until the next
// window's From. From is inclusive; the zero value means "since the beginning", the
// open-ended first window every model has. A model's windows are sorted by From
// ascending, so a lookup walks them and keeps the last one whose From is at or
// before the event time.
type DatedRate struct {
	From time.Time // inclusive lower bound; zero value = in effect from the beginning
	Rate Rate
}

// flat wraps a single rate as a one-window list in effect for all time, the shape of
// a model whose price has never changed. It keeps the common table entry a bare Rate
// literal rather than a DatedRate slice.
func flat(r Rate) []DatedRate { return []DatedRate{{Rate: r}} }

// sonnet5Sticker is the date Claude Sonnet 5's introductory $2/$10 promo ends and the
// $3/$15 sticker rate takes over. It is a UTC-midnight boundary so it aligns with the
// day buckets the aggregate cache-savings paths price against (see store/analytics_cache.go).
var sonnet5Sticker = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// table maps a canonical model ID to its date-effective rates. Matching is EXACT, not
// by prefix: a key prices only the model whose ID it is, never a whole family or major
// line. That is deliberate. Pricing has diverged within a major line before (Opus 4.1
// at $15/$75 then Opus 4.5 at $5/$25; GPT-5 at $1.25/$10 then GPT-5.5 at $5/$30), but
// each of those was a NEW model ID, so exact-match keys already price them apart. The
// date dimension here is the other axis: a rate that changes on a date under ONE ID (a
// promo that reverts, a mid-life reprice), which a second key cannot express.
//
// A prefix that looked uniform today would silently misprice the next version that
// repriced. With exact matching that whole bug class is impossible: a model we have not
// listed (a new minor, a new variant) falls through to known=false and is reported as
// an incomplete cost rather than a wrong number.
//
// Keys are the canonical, dateless IDs. Lookup strips a trailing release-date
// snapshot before matching (see datedSnapshot), so both the alias
// (claude-opus-4-8) and the dated ID (claude-opus-4-8-20260115) resolve to one
// key. A model whose dateless ID carries no minor number (Opus 4.0's
// claude-opus-4-20250514 normalizes to "claude-opus-4") is keyed by that bare
// ID; under exact matching that is the model's own name, not a catch-all.
//
// Most models carry one flat window. A model with more than one lists its windows
// From-ascending, the first with a zero From (in effect from the beginning);
// TestTableWindowsSorted guards that shape. TestUnlistedModelsAreUnknown guards that
// future and sibling models stay unknown. When adding a model, add its exact ID; do
// not widen an existing key.
var table = map[string][]DatedRate{
	// Fable 5 and Mythos 5 share pricing; mythos-preview is the invitation-only
	// predecessor at the same rate.
	"claude-fable-5":        flat(Rate{Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00}),
	"claude-mythos-5":       flat(Rate{Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00}),
	"claude-mythos-preview": flat(Rate{Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00}),

	// Opus: 4.0/4.1 at $15/$75, 4.5 onward at $5/$25. "claude-opus-4" is Opus
	// 4.0's dateless ID (claude-opus-4-20250514 normalizes to it).
	"claude-opus-4":   flat(Rate{Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50}),
	"claude-opus-4-0": flat(Rate{Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50}),
	"claude-opus-4-1": flat(Rate{Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50}),
	"claude-opus-4-5": flat(Rate{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50}),
	"claude-opus-4-6": flat(Rate{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50}),
	"claude-opus-4-7": flat(Rate{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50}),
	"claude-opus-4-8": flat(Rate{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50}),

	// Sonnet: $3/$15 from 3.5 through 5, except Sonnet 5's launch promo. Sonnet 5
	// launched at an introductory $2/$10 per MTok through 2026-08-31 and reverts to
	// the $3/$15 sticker on 2026-09-01, so it carries two windows; the cache rates
	// track input at the usual Anthropic ratios (write 1.25x, read 0.1x). Everything
	// else is a single flat window. "claude-sonnet-4" is Sonnet 4.0's dateless ID
	// (claude-sonnet-4-20250514 normalizes to it).
	"claude-sonnet-5": {
		{Rate: Rate{Input: 2, Output: 10, CacheWrite: 2.50, CacheRead: 0.20}},
		{From: sonnet5Sticker, Rate: Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}},
	},
	"claude-sonnet-4":   flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),
	"claude-sonnet-4-0": flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),
	"claude-sonnet-4-5": flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),
	"claude-sonnet-4-6": flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),
	"claude-3-7-sonnet": flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),
	"claude-3-5-sonnet": flat(Rate{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}),

	// Haiku: 4.5 is the only 4.x; 3.5 is the prior generation.
	"claude-haiku-4-5": flat(Rate{Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10}),
	"claude-3-5-haiku": flat(Rate{Input: 0.80, Output: 4, CacheWrite: 1, CacheRead: 0.08}),

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
	"gpt-5.5":       flat(Rate{Input: 5, Output: 30, CacheRead: 0.50}),
	"gpt-5.5-pro":   flat(Rate{Input: 30, Output: 180}),
	"gpt-5.4":       flat(Rate{Input: 2.50, Output: 15, CacheRead: 0.25}),
	"gpt-5.4-mini":  flat(Rate{Input: 0.75, Output: 4.50, CacheRead: 0.075}),
	"gpt-5.4-nano":  flat(Rate{Input: 0.20, Output: 1.25, CacheRead: 0.02}),
	"gpt-5.4-pro":   flat(Rate{Input: 30, Output: 180}),
	"gpt-5.3-codex": flat(Rate{Input: 1.75, Output: 14, CacheRead: 0.175}),
	// Prior generation (GPT-5, Aug 2025 launch). gpt-5-2025-08-07 normalizes to
	// "gpt-5"; under exact matching that base key does not absorb gpt-5.4/gpt-5.5.
	"gpt-5":       flat(Rate{Input: 1.25, Output: 10, CacheRead: 0.125}),
	"gpt-5-codex": flat(Rate{Input: 1.25, Output: 10, CacheRead: 0.125}),
	"gpt-5-mini":  flat(Rate{Input: 0.25, Output: 2, CacheRead: 0.025}),
	"gpt-5-nano":  flat(Rate{Input: 0.05, Output: 0.40, CacheRead: 0.005}),
}

// datedSnapshot matches a trailing release-date suffix in either the Anthropic
// form (-20250514) or the OpenAI form (-2025-08-07). Stripping it maps a dated
// model ID back to its canonical key so both forms price identically.
var datedSnapshot = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

// normalize lowercases, trims, and strips a trailing release-date snapshot from a
// model ID so both the alias and the dated form match one table key. It returns the
// empty string for input that normalizes to nothing (a bare date, whitespace), which
// the callers treat as unknown.
func normalize(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return datedSnapshot.ReplaceAllString(model, "")
}

// rateAt returns the rate in effect at time `at`, walking the From-ascending windows
// and keeping the last one whose From is at or before `at`. A zero `at` (an undated
// usage event) selects the first window, which is the flat rate for a single-window
// model and the earliest window for a multi-window one; parse-time pricing makes the
// same choice for an undated row, so a rollup and a from-scratch recompute agree.
func rateAt(rates []DatedRate, at time.Time) Rate {
	r := rates[0].Rate
	for _, dr := range rates[1:] {
		if dr.From.After(at) {
			break
		}
		r = dr.Rate
	}
	return r
}

// RateAt returns the rate for a model at a point in time, and whether it was found.
// The model string is normalized (lowercased, trimmed, and stripped of a trailing
// release-date snapshot) and then matched exactly against the table. There is no prefix
// matching: a key prices only its exact model, so a model we have not listed reports
// known=false rather than inheriting a neighbor's price. The time selects the
// date-effective window (see rateAt).
func RateAt(model string, at time.Time) (Rate, bool) {
	model = normalize(model)
	if model == "" {
		return Rate{}, false
	}
	rates, ok := table[model]
	if !ok {
		return Rate{}, false
	}
	return rateAt(rates, at), true
}

// Known reports whether a model is priced at all, independent of any date. A model's
// windows all share one ID, so its presence in the table does not depend on when it ran;
// a by-model view that only needs to fold unpriced models into an "Other" bucket asks
// this rather than picking an arbitrary window's rate.
func Known(model string) bool {
	m := normalize(model)
	if m == "" {
		return false
	}
	_, ok := table[m]
	return ok
}

// Cost returns the USD cost for a token count under a model at the time the usage
// occurred, and whether the model was priced. Token counts are in tokens (not millions).
// The time selects the date-effective rate window.
func Cost(model string, at time.Time, input, output, cacheWrite, cacheRead int) (float64, bool) {
	r, ok := RateAt(model, at)
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
// The time selects the date-effective rate window, so cached volume prices at the rate
// in effect when it was spent.
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
// past a 32-bit range, where Cost only ever sees a single session's tokens. A caller
// that rolls many events into one figure must bucket them so every event in a bucket
// falls in one rate window (see store/analytics_cache.go), since a single time picks a
// single window for the whole sum.
func CacheSavings(model string, at time.Time, cacheRead, cacheWrite int64) (float64, bool) {
	r, ok := RateAt(model, at)
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	saving := float64(cacheRead)/million*(r.Input-r.CacheRead) +
		float64(cacheWrite)/million*(r.Input-r.CacheWrite)
	return saving, true
}
