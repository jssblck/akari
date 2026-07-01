package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertHygieneSignal writes a session_signals row with the prompt-hygiene columns set, so
// a cohort test can pin the aggregate without driving the whole classification (signals_test
// already covers that path). The outcome is a valid placeholder; score and grade are left
// unscored, since the hygiene aggregate never reads them.
func insertHygieneSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, version, promptCount, short, dup, nocode int, unstructured bool) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals
		   (session_id, signals_version, outcome, outcome_confidence,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start)
		 VALUES ($1, $2, 'completed', 'high', $3, $4, $5, $6, $7)`,
		sid, version, promptCount, short, dup, nocode, unstructured); err != nil {
		t.Fatalf("insert hygiene signal for session %d: %v", sid, err)
	}
}

// TestPromptHygiene pins the cohort aggregate: it sums the per-session hygiene counts over
// the sessions carrying a current-version signals row, using the session's human-prompt
// count as the rate denominator, and honors the window and per-user scoping. A stale-version
// row is excluded so the panel never mixes a half-rebuilt view.
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
	// prompt_count) and a hygiene row.
	seed := func(user int64, src string, started time.Time, version, prompts, short, dup, nocode int, unstructured bool) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), prompts*3, prompts)
		insertHygieneSignal(t, st, ctx, sid, version, prompts, short, dup, nocode, unstructured)
	}

	seed(ada, "h1", recent, quality.Version, 5, 2, 1, 1, true)     // in window, current
	seed(ada, "h2", recent, quality.Version, 3, 0, 0, 2, false)    // in window, current
	seed(grace, "h3", recent, quality.Version, 4, 1, 0, 0, true)   // in window, current, other user
	seed(grace, "h4", old, quality.Version, 6, 3, 1, 1, true)      // out of window, current
	seed(ada, "h5stale", recent, quality.Version+999, 9, 9, 9, 9, true) // in window, stale -> excluded

	// Unscoped: every current-version row regardless of start (h1..h4); the stale row drops.
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

	// Ada only: her two current-version sessions (h1, h2); the stale h5 stays excluded.
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
