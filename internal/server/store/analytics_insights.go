package store

import "context"

// Insights is everything the Insights page renders for a scope: the quality
// distribution (grades and outcomes) and the archetype mix. It is the cross-cutting
// counterpart to Analytics (which is about cost and tokens), scoped by the same
// AnalyticsFilter so a window or a per-user narrowing applies to both surfaces alike.
// Later slices extend it with the concurrency timeline and velocity percentiles.
type Insights struct {
	Quality    QualityDistribution
	Archetypes []LabeledCount
}

// HasData reports whether any scoped session carried signals, so the page can show an
// empty state instead of a row of zero bars on a scope with no sessions.
func (i Insights) HasData() bool { return i.Quality.Sessions > 0 }

// Insights gathers the page's panels in one call. Each panel is an independent scoped
// aggregate (no shared base to reconcile, unlike the cost analytics), so they run in
// sequence and fail fast on the first error.
func (s *Store) Insights(ctx context.Context, f AnalyticsFilter) (Insights, error) {
	quality, err := s.QualityDistribution(ctx, f)
	if err != nil {
		return Insights{}, err
	}
	archetypes, err := s.ArchetypeDistribution(ctx, f)
	if err != nil {
		return Insights{}, err
	}
	return Insights{Quality: quality, Archetypes: archetypes}, nil
}
