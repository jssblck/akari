package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestWindowSessionPageUsesOneSnapshot moves the hidden boundary session while
// the page is between its visible-row and remainder reads. Both pieces must
// retain the ordering from the first read, or the boundary row is counted twice
// and the moved row disappears from the page totals.
func TestWindowSessionPageUsesOneSnapshot(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "anna_winlock", "h", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/anna/snapshot", "github.com", "anna", "snapshot", "snapshot", "remote")
	if err != nil {
		t.Fatal(err)
	}

	const total = 101
	base := time.Now().Add(-24 * time.Hour)
	var hiddenID int64
	for i := 0; i < total; i++ {
		sid := seedSessionWithStats(t, st, user.ID, projectID, "claude", fmt.Sprintf("snapshot-%03d", i), 0, 0, 0)
		input := int64(10 + i)
		if i == 0 {
			hiddenID = sid
			input = 777
		}
		seedUsageAt(t, st, sid, "claude-opus-4-8", 1, input, 1, base, fmt.Sprintf("snapshot-usage-%03d", i))
		if _, err := st.Pool.Exec(ctx,
			"UPDATE sessions SET ended_at = $2 WHERE id = $1",
			sid, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("order session %d: %v", i, err)
		}
	}

	rowsRead := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	defer release()
	st.SetWindowSessionRowsReadHookForTest(func() {
		close(rowsRead)
		<-resume
	})

	type result struct {
		page store.SessionPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: projectID})
		done <- result{page: page, err: err}
	}()

	select {
	case <-rowsRead:
	case <-time.After(5 * time.Second):
		t.Fatal("visible session query did not reach the snapshot seam")
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE sessions SET ended_at = $2 WHERE id = $1",
		hiddenID, base.Add(48*time.Hour)); err != nil {
		t.Fatalf("move hidden session across boundary: %v", err)
	}
	release()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("window session page did not finish")
	}
	if got.err != nil {
		t.Fatalf("window session page: %v", got.err)
	}
	if len(got.page.Sessions) != 100 {
		t.Fatalf("visible sessions = %d, want 100", len(got.page.Sessions))
	}
	for _, session := range got.page.Sessions {
		if session.ID == hiddenID {
			t.Fatalf("session %d moved into a later snapshot's visible set", hiddenID)
		}
	}
	if got.page.Remainder.Sessions != 1 || got.page.Remainder.Input != 777 {
		t.Fatalf("remainder = %+v, want the original hidden session with 777 input tokens", got.page.Remainder)
	}
}
