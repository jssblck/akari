package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestContextMessageExcludedFromTitle pins that a context-role message (the injected
// agent framing the Codex reducer now classifies as RoleContext) never becomes the
// session title: the title lateral filters on role='user', so it walks past the
// context turn to the real opening prompt. The context body opens with the AGENTS.md
// marker, so a regression that re-roled it back to 'user' (or dropped the role filter
// on the lateral) would leak that framing text into the title here. The count rollups
// are folded on the ingest path, not by ApplyProjectionDelta, so their exclusion is
// covered end-to-end in the parse package's TestCodexContextExcludedFromCounts.
func TestContextMessageExcludedFromTitle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	// Ordinal 0 is injected context (as Codex prepends it); ordinal 1 is the real prompt; ordinal 2
	// is the assistant reply.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "context", Content: "# AGENTS.md instructions for /home/ada/akari\n\nRun make build."},
			{Ordinal: 1, Role: "user", Content: "Add rate limiting to the API"},
			{Ordinal: 2, Role: "assistant", Content: "On it.", Model: "gpt-5-codex"},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	d, err := st.SessionDetailByID(ctx, sid)
	if err != nil {
		t.Fatalf("session detail: %v", err)
	}
	if d.Title != "Add rate limiting to the API" {
		t.Errorf("title = %q, want the real opening prompt, not the injected context", d.Title)
	}
}
