// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// FallbacksByOrdinal indexes a session's fallbacks by the transcript message ordinal
// they landed on, for the inline transcript notice to look up a turn in O(1) while the
// walker renders messages. A fallback whose source lines never named a message ordinal
// is left out: it has no turn to mark, so it appears only in the header tile's tooltip.
// When two fallbacks share one ordinal (a turn declined twice), the first in occurrence
// order wins the slot, since the slice arrives already ordered by occurrence.
func FallbacksByOrdinal(fbs []store.ModelFallback) map[int]store.ModelFallback {
	if len(fbs) == 0 {
		return nil
	}
	out := make(map[int]store.ModelFallback, len(fbs))
	for _, f := range fbs {
		if f.MessageOrdinal == nil {
			continue
		}
		if _, seen := out[*f.MessageOrdinal]; seen {
			continue
		}
		out[*f.MessageOrdinal] = f
	}
	return out
}

// FallbackBadgeLabel is the feed row's compact tag text: "fallback" for one, "fallback
// xN" for more (with the multiplication sign, not the letter). The badge is a quiet
// count; the fuller sentence lives in its title.
func FallbackBadgeLabel(count int) string {
	if count <= 1 {
		return "fallback"
	}
	return fmt.Sprintf("fallback ×%d", count)
}

// FallbackBadgeTitle is the feed badge's hover sentence, naming what the count means in
// plain terms. It states the fact ("fell back from Fable 5 to a lower model") without
// naming the served model, which the feed row does not carry; the session page names it.
func FallbackBadgeTitle(count int) string {
	if count <= 1 {
		return "1 turn fell back from Fable 5 to a lower model"
	}
	return fmt.Sprintf("%d turns fell back from Fable 5 to a lower model", count)
}

// FallbackModelsLabel renders one fallback's model pair for a tooltip row, using a real
// arrow glyph between the from-model and the served model. Either side may be unset when
// only one transcript line of the fallback was seen, so a missing model reads "unknown"
// rather than an empty run.
func FallbackModelsLabel(f store.ModelFallback) string {
	from := f.FromModel
	if from == "" {
		from = "unknown"
	}
	to := f.ToModel
	if to == "" {
		to = "unknown"
	}
	return from + " → " + to
}

// FallbackCategoryLabel renders one fallback's refusal category for a tooltip row. The
// category is empty until the system entry of the fallback merged in, so an unset value
// reads "uncategorized" rather than blank.
func FallbackCategoryLabel(f store.ModelFallback) string {
	if f.RefusalCategory == "" {
		return "uncategorized"
	}
	return f.RefusalCategory
}

// FallbackDeclinedObserved reports whether the declined attempt's spend was fully measured:
// all four token classes merged in from the assistant source line. The classes arrive
// together, so a single nil means the spend was never observed and the tooltip shows no
// declined figures rather than a partial, misleading breakdown. This matches the MCP DTO,
// which likewise reports the declined tokens only when every class is present.
func FallbackDeclinedObserved(f store.ModelFallback) bool {
	return f.DeclinedInput != nil && f.DeclinedOutput != nil &&
		f.DeclinedCacheWrite != nil && f.DeclinedCacheRead != nil
}

// FallbacksOverflow reports how many fallbacks the count claims beyond the shown rows, so a
// tooltip that renders only a leading window can name the remainder in a "plus N more" line.
// It is never negative: the count is the session-wide total and shown is bounded by it.
func FallbacksOverflow(count, shown int) int {
	if count <= shown {
		return 0
	}
	return count - shown
}

// FallbackTimeLabel renders when a fallback occurred for a tooltip row, in the viewer's
// timezone, or a dash when the source lines carried no timestamp.
func FallbackTimeLabel(ctx context.Context, f store.ModelFallback) string {
	if f.OccurredAt == nil {
		return "-"
	}
	return FmtTime(ctx, f.OccurredAt)
}

// FallbackNoticeLabel is the inline transcript notice above the turn a fallback landed
// on: a plain sentence naming the model pair, with the refusal category (or trigger) in
// parentheses when either is known. Models fall back to "unknown" the same way the tile
// does, so the notice never reads a blank model.
func FallbackNoticeLabel(f store.ModelFallback) string {
	from := f.FromModel
	if from == "" {
		from = "unknown"
	}
	to := f.ToModel
	if to == "" {
		to = "unknown"
	}
	// The category (e.g. reasoning_extraction) names what tripped the classifier and
	// reads better inline than the generic mechanism ("refusal"), so it wins when known.
	reason := f.RefusalCategory
	if reason == "" {
		reason = f.Trigger
	}
	if reason == "" {
		return fmt.Sprintf("Fell back from %s to %s", from, to)
	}
	return fmt.Sprintf("Fell back from %s to %s (%s)", from, to, reason)
}
