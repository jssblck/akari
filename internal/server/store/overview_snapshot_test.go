package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestProjectOverviewSnapshotCarriesSharedPublicAndAuthenticatedShape(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sessionID := seedSessionWithStats(t, st, u.ID, projectID, "codex", "shared-overview", 1.5, 120, 80)
	seedUsage(t, st, sessionID, "gpt-5.5", 1.5, 120, 80, 1, "shared-overview-usage")

	analytics, insights, err := st.ProjectOverviewSnapshot(ctx, store.AnalyticsFilter{
		ProjectID: projectID,
		Since:     time.Now().AddDate(0, 0, -30),
		Until:     endOfTodayUTC(),
		Bucket:    "day",
	})
	if err != nil {
		t.Fatalf("ProjectOverviewSnapshot: %v", err)
	}
	if analytics.TotalIn != 120 || analytics.TotalOut != 80 || analytics.Sessions != 1 {
		t.Fatalf("analytics = (in %d, out %d, sessions %d), want (120, 80, 1)", analytics.TotalIn, analytics.TotalOut, analytics.Sessions)
	}
	if len(analytics.Users) != 1 || analytics.Users[0].Label != "ada" {
		t.Fatalf("by-user shape = %+v, want one ada row for the authenticated view", analytics.Users)
	}
	if insights.Trends == nil || insights.Trends.Unit != "day" {
		t.Fatalf("insights trends = %+v, want shared day-bucket shape", insights.Trends)
	}
}
