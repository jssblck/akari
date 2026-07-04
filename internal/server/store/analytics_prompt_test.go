package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertHygieneSignal writes a session_signals row with the prompt-hygiene columns set, so
// a cohort test can pin the aggregate without driving the whole classification (signals_test
// already covers that path). The outcome is a valid placeholder; score and grade are left
// unscored, since the hygiene aggregate never reads them. fresh controls whether signals_stale
// is cleared afterward, standing in for a session whose grade the fleet read gate should see
// (true) versus one whose projection moved since it was graded (false).
func insertHygieneSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, fresh bool, promptCount, short, dup, nocode int, unstructured bool) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals
		   (session_id, outcome, outcome_confidence,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start)
		 VALUES ($1, 'completed', 'high', $2, $3, $4, $5, $6)`,
		sid, promptCount, short, dup, nocode, unstructured); err != nil {
		t.Fatalf("insert hygiene signal for session %d: %v", sid, err)
	}
	if fresh {
		markSignalsFresh(t, st, ctx, sid)
	}
}

// TestPromptHygiene pins the cohort aggregate: it sums the per-session hygiene counts over
// the sessions carrying a fresh signals row, using the session's human-prompt count as the
// rate denominator, and honors the window and per-user scoping. A row whose signals_stale
// flag is still set is excluded so the panel never mixes a half-rebuilt view: a grade whose
// projection has since moved drops out until the rebuild re-derives it.
func TestPromptHygiene(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-1 * time.Hour)
	old := time.Now().Add(-400 * 24 * time.Hour)

	// seed a session with a set human-prompt count (the classifier base, stored as
	// prompt_count) and a hygiene row, fresh or stale.
	seed := func(user int64, src string, started time.Time, fresh bool, prompts, short, dup, nocode int, unstructured bool) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), prompts*3, prompts)
		insertHygieneSignal(t, st, ctx, sid, fresh, prompts, short, dup, nocode, unstructured)
	}

	seed(ada, "h1", recent, true, 5, 2, 1, 1, true)       // in window, fresh
	seed(ada, "h2", recent, true, 3, 0, 0, 2, false)      // in window, fresh
	seed(grace, "h3", recent, true, 4, 1, 0, 0, true)     // in window, fresh, other user
	seed(grace, "h4", old, true, 6, 3, 1, 1, true)        // out of window, fresh
	seed(ada, "h5stale", recent, false, 9, 9, 9, 9, true) // graded but the projection moved since -> excluded

	// Unscoped: every fresh row regardless of start (h1..h4); the stale row drops.
	all, err := st.PromptHygiene(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("prompt hygiene (all): %v", err)
	}
	if !all.HasData() {
		t.Fatal("unscoped hygiene should have data")
	}
	if all.Prompts != 18 || all.Short != 6 || all.Duplicate != 2 || all.NoCodeContext != 4 ||
		all.Sessions != 4 || all.UnstructuredStarts != 3 {
		t.Errorf("unscoped = %+v, want {Prompts 18, Short 6, Duplicate 2, NoCodeContext 4, Sessions 4, UnstructuredStarts 3}", all)
	}

	// Windowed: a trailing window drops the old session (h4).
	windowed, err := st.PromptHygiene(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("prompt hygiene (windowed): %v", err)
	}
	if windowed.Prompts != 12 || windowed.Short != 3 || windowed.Duplicate != 1 || windowed.NoCodeContext != 3 ||
		windowed.Sessions != 3 || windowed.UnstructuredStarts != 2 {
		t.Errorf("windowed = %+v, want {Prompts 12, Short 3, Duplicate 1, NoCodeContext 3, Sessions 3, UnstructuredStarts 2}", windowed)
	}

	// Ada only: her two fresh sessions (h1, h2); the stale h5 stays excluded.
	adaOnly, err := st.PromptHygiene(ctx, store.AnalyticsFilter{Username: "ada"})
	if err != nil {
		t.Fatalf("prompt hygiene (ada): %v", err)
	}
	if adaOnly.Prompts != 8 || adaOnly.Short != 2 || adaOnly.Duplicate != 1 || adaOnly.NoCodeContext != 3 ||
		adaOnly.Sessions != 2 || adaOnly.UnstructuredStarts != 1 {
		t.Errorf("ada = %+v, want {Prompts 8, Short 2, Duplicate 1, NoCodeContext 3, Sessions 2, UnstructuredStarts 1}", adaOnly)
	}
}

// TestPromptHygieneEmpty confirms a scope with no signalled session reports no data, so the
// panel shows a note rather than a row of zero-over-zero rates.
func TestPromptHygieneEmpty(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	h, err := st.PromptHygiene(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("prompt hygiene (empty): %v", err)
	}
	if h.HasData() {
		t.Errorf("empty scope should have no data, got %+v", h)
	}
}
