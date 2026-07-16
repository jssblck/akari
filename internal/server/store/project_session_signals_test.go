package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestWindowSessionPageSurfacesOnlyCurrentSignals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/ada/signals", "github.com", "ada", "signals", "signals", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	now := time.Now()
	currentID := seedSessionWithStats(t, st, user.ID, projectID, "claude", "current", 1, 10, 5)
	seedUsageAt(t, st, currentID, "claude-opus-4-8", 1, 10, 5, now.Add(-time.Hour), "current")
	insertSignal(t, st, ctx, currentID, "completed", "B")

	staleID := seedSessionWithStats(t, st, user.ID, projectID, "claude", "stale", 1, 10, 5)
	seedUsageAt(t, st, staleID, "claude-opus-4-8", 1, 10, 5, now.Add(-2*time.Hour), "stale")
	insertStaleSignal(t, st, ctx, staleID, "abandoned", "A")

	page, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("window session page: %v", err)
	}
	if len(page.Sessions) != 2 {
		t.Fatalf("want 2 windowed rows, got %d", len(page.Sessions))
	}

	rows := make(map[int64]store.ProjectSessionSummary, len(page.Sessions))
	for _, row := range page.Sessions {
		rows[row.ID] = row
	}
	current := rows[currentID]
	if current.Grade == nil || *current.Grade != "B" || current.Outcome != "completed" {
		t.Errorf("current signals = grade %v outcome %q, want B/completed", current.Grade, current.Outcome)
	}
	stale := rows[staleID]
	if stale.Grade != nil || stale.Outcome != "" {
		t.Errorf("stale signals = grade %v outcome %q, want no current verdict", stale.Grade, stale.Outcome)
	}
}
