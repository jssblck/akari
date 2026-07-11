package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectOverviewSnapshot reads the usage panel and quality band from one
// repeatable-read transaction. Public and unfiltered authenticated project views
// share the completed result in the HTTP snapshot cache, so this refresh path favors
// one immutable database generation over the ordinary Insights fan-out across
// several imported snapshot transactions.
func (s *Store) ProjectOverviewSnapshot(ctx context.Context, f AnalyticsFilter) (Analytics, Insights, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Analytics{}, Insights{}, fmt.Errorf("project overview snapshot: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	analyticsFilter := f
	analyticsFilter.Bucket = ""
	analytics, err := s.analyticsFrom(ctx, tx, analyticsFilter)
	if err != nil {
		return Analytics{}, Insights{}, fmt.Errorf("project overview snapshot: analytics: %w", err)
	}

	insightsFilter := f
	var grid trendGrid
	var wantTrends bool
	var insights Insights
	if f.Bucket != "" {
		now := time.Now()
		since, err := s.resolveTrendSince(ctx, tx, f, now)
		if err != nil {
			return Analytics{}, Insights{}, fmt.Errorf("project overview snapshot: trend range: %w", err)
		}
		grid = newTrendGrid(f.Bucket, since, now)
		if grid.n() > 0 {
			if grid.Starts[0].After(insightsFilter.Since) {
				insightsFilter.Since = grid.Starts[0]
			}
			if upper := advanceBucket(grid.Unit, grid.Starts[grid.n()-1]); insightsFilter.Until.IsZero() || upper.Before(insightsFilter.Until) {
				insightsFilter.Until = upper
			}
			wantTrends = true
		}
		insights.Trends = &Trends{Unit: grid.Unit, BucketStarts: grid.Starts, Labels: grid.labels()}
	}
	for _, read := range s.insightsReads(insightsFilter, QualityBandPanels, grid, wantTrends, &insights) {
		if err := read(ctx, tx); err != nil {
			return Analytics{}, Insights{}, fmt.Errorf("project overview snapshot: insights: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Analytics{}, Insights{}, fmt.Errorf("project overview snapshot: commit: %w", err)
	}
	return analytics, insights, nil
}
