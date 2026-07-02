package store

import (
	"context"
	"fmt"

	"github.com/jssblck/akari/internal/quality"
)

// archetypeCaseExpr is the SQL banding for session archetypes, built once from the
// quality package's thresholds so the database and quality.ClassifyArchetype share one
// numeric source. It mirrors that function exactly: automation first (no human turn),
// then the heaviest band whose duration-in-minutes OR message count it reaches. A
// session with no start/end span has a NULL duration, coalesced to 0 so it bands on its
// message count alone. The constants are our own ints, so interpolation is safe.
var archetypeCaseExpr = fmt.Sprintf(`CASE
	  WHEN s.user_message_count = 0 THEN 'automation'
	  WHEN coalesce(extract(epoch FROM (s.ended_at - s.started_at)) / 60, 0) >= %d OR s.message_count >= %d THEN 'marathon'
	  WHEN coalesce(extract(epoch FROM (s.ended_at - s.started_at)) / 60, 0) >= %d OR s.message_count >= %d THEN 'deep'
	  WHEN coalesce(extract(epoch FROM (s.ended_at - s.started_at)) / 60, 0) >= %d OR s.message_count >= %d THEN 'standard'
	  ELSE 'quick' END`,
	quality.MarathonMinutes, quality.MarathonMessages,
	quality.DeepMinutes, quality.DeepMessages,
	quality.StandardMinutes, quality.StandardMessages)

// archetypeOrder fixes the bar order from lightest to heaviest, with automation last as
// the non-human catch-all, so the distribution reads as a spectrum of session weight.
var archetypeOrder = []string{
	string(quality.ArchetypeQuick),
	string(quality.ArchetypeStandard),
	string(quality.ArchetypeDeep),
	string(quality.ArchetypeMarathon),
	string(quality.ArchetypeAutomation),
}

// ArchetypeDistribution buckets the scoped sessions by shape on its own pooled connection
// for the Insights page. The snapshot path threads archetypeDistributionFrom so every
// panel reads one MVCC snapshot.
func (s *Store) ArchetypeDistribution(ctx context.Context, f AnalyticsFilter) ([]LabeledCount, error) {
	return s.archetypeDistributionFrom(ctx, s.Pool, f)
}

// archetypeDistributionFrom buckets the scoped sessions by shape (quick / standard / deep /
// marathon / automation) for the Insights page. It reads straight from sessions (the
// facts are columns there: user turns, message count, and the start/end span), scoped
// by the shared analytics filter on s.started_at, so the same window and narrowing the
// usage panel uses applies here. One GROUP BY over the banding CASE, folded into the
// fixed order with zero-filled buckets.
func (s *Store) archetypeDistributionFrom(ctx context.Context, q querier, f AnalyticsFilter) ([]LabeledCount, error) {
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx,
		`SELECT `+archetypeCaseExpr+`, count(*)
		   FROM sessions s
		  WHERE TRUE`+filter+`
		  GROUP BY 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("archetype distribution: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("scan archetype distribution: %w", err)
		}
		counts[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate archetype distribution: %w", err)
	}
	return orderedCounts(archetypeOrder, counts), nil
}
