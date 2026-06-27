// Package pricing computes session cost from a model rate table compiled into
// the binary. There is no runtime catalog or refresh: updating rates means a new
// build. Rates are a snapshot in USD per one million tokens and are intentionally
// approximate; an unknown model yields known=false so callers can mark a cost as
// partial rather than reporting a misleading zero.
package pricing

import "strings"

// Rate holds per-million-token prices for one model family.
type Rate struct {
	Input      float64
	Output     float64
	CacheWrite float64 // cache creation
	CacheRead  float64
}

// table maps a model-name prefix to its rate. Lookups choose the longest
// matching prefix, so more specific families (gpt-5-codex) win over general ones
// (gpt-5).
var table = map[string]Rate{
	"claude-opus-4":     {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-3-7-sonnet": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-3-5-sonnet": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-haiku-4":    {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
	"claude-3-5-haiku":  {Input: 0.80, Output: 4, CacheWrite: 1, CacheRead: 0.08},
	"gpt-5-codex":       {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-mini":        {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5":             {Input: 1.25, Output: 10, CacheRead: 0.125},
}

// Lookup returns the rate for a model and whether it was found.
func Lookup(model string) (Rate, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return Rate{}, false
	}
	best := ""
	for prefix := range table {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return Rate{}, false
	}
	return table[best], true
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
