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
// unscored, since the hygiene aggregate never reads them. factsVersion is the
// prompt_facts_version stamp the read gate checks alongside signals_version, so a test can seed a
// row at the current signals_version but a superseded classifier version to exercise that gate.
func insertHygieneSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, version, factsVersion, promptCount, short, dup, nocode int, unstructured bool) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals
		   (session_id, signals_version, prompt_facts_version, outcome, outcome_confidence,
		    prompt_count, short_prompt_count, duplicate_prompt_count, no_code_context_count, unstructured_start)
		 VALUES ($1, $2, $3, 'completed', 'high', $4, $5, $6, $7, $8)`,
		sid, version, factsVersion, promptCount, short, dup, nocode, unstructured); err != nil {
		t.Fatalf("insert hygiene signal for session %d: %v", sid, err)
	}
	markSignalsFresh(t, st, ctx, sid)
}

// TestPromptHygiene pins the cohort aggregate: it sums the per-session hygiene counts over
// the sessions carrying a current-version signals row, using the session's human-prompt
// count as the rate denominator, and honors the window and per-user scoping. A row stale on
// either version axis is excluded so the panel never mixes a half-rebuilt view: a stale
// signals_version (a scoring change) and a stale prompt_facts_version (a classifier change, at
// the current signals_version) both drop out until the reparse re-derives them.
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
	// prompt_count) and a hygiene row at the given signals and classifier versions.
	seed := func(user int64, src string, started time.Time, version, factsVersion, prompts, short, dup, nocode int, unstructured bool) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), prompts*3, prompts)
		insertHygieneSignal(t, st, ctx, sid, version, factsVersion, prompts, short, dup, nocode, unstructured)
	}

	cur, curFacts := quality.Version, quality.PromptFactsVersion
	seed(ada, "h1", recent, cur, curFacts, 5, 2, 1, 1, true)               // in window, current
	seed(ada, "h2", recent, cur, curFacts, 3, 0, 0, 2, false)              // in window, current
	seed(grace, "h3", recent, cur, curFacts, 4, 1, 0, 0, true)             // in window, current, other user
	seed(grace, "h4", old, cur, curFacts, 6, 3, 1, 1, true)                // out of window, current
	seed(ada, "h5stale", recent, cur+999, curFacts, 9, 9, 9, 9, true)      // stale signals_version -> excluded
	seed(ada, "h6factsstale", recent, cur, curFacts+999, 9, 9, 9, 9, true) // stale prompt_facts_version -> excluded

	// Unscoped: every fully-current row regardless of start (h1..h4); both stale rows drop.
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

	// Ada only: her two fully-current sessions (h1, h2); the stale-signals h5 and stale-facts h6
	// both stay excluded, so the prompt_facts_version gate holds even at the current signals_version.
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
