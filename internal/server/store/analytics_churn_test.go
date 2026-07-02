package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// churnByPath finds a file's churn row, failing the test when it is absent.
func churnByPath(t *testing.T, files []store.ChurnFile, path string) store.ChurnFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("path %q not found in churn %+v", path, files)
	return store.ChurnFile{}
}

// TestFileChurn pins the edit-thrash aggregate: only files edited more than once appear,
// counts are deduped (a replayed edit collapses), an edit with no parsed path is ignored,
// the session count spans the fleet, and the list orders by edit count. It also confirms
// the window and per-user scoping narrow the set.
func TestFileChurn(t *testing.T) {
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

	// Ada: three edits to a.go (one replayed across a later turn, which collapses), one to
	// b.go (not churn), and one edit whose path did not parse (ignored).
	s1 := seedSession(t, st, ada, pid, "ch1")
	if err := st.ApplyProjectionDelta(ctx, s1, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit"},
			{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", Content: "replay", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "a1", CallUID: "e1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "a2", CallUID: "e2"},
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "a3", CallUID: "e3"},
			{MessageOrdinal: 1, CallIndex: 3, ToolName: "Edit", Category: "edit", FilePath: "b.go", InputBody: "b1", CallUID: "e4"},
			{MessageOrdinal: 1, CallIndex: 4, ToolName: "Edit", Category: "edit", InputBody: "np", CallUID: "en"}, // no path -> ignored
			// e1 replayed verbatim in a later turn: collapses with the original.
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "a1", CallUID: "e1"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "e1", Status: "ok"}, {CallUID: "e2", Status: "ok"}, {CallUID: "e3", Status: "ok"},
			{CallUID: "e4", Status: "ok"}, {CallUID: "en", Status: "ok"},
		},
	}); err != nil {
		t.Fatalf("apply s1: %v", err)
	}
	setSessionShape(t, st, ctx, s1, recent, recent.Add(10*time.Minute), 6, 2)

	// Grace: one more edit to a.go (a second session touches it) and two to c.go. Started
	// long ago, so a window drops it.
	s2 := seedSession(t, st, grace, pid, "ch2")
	if err := st.ApplyProjectionDelta(ctx, s2, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit"},
			{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "ga1", CallUID: "g1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "c.go", InputBody: "gc1", CallUID: "g2"},
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Edit", Category: "edit", FilePath: "c.go", InputBody: "gc2", CallUID: "g3"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "g1", Status: "ok"}, {CallUID: "g2", Status: "ok"}, {CallUID: "g3", Status: "ok"},
		},
	}); err != nil {
		t.Fatalf("apply s2: %v", err)
	}
	setSessionShape(t, st, ctx, s2, old, old.Add(5*time.Minute), 2, 1)

	// Unscoped: a.go (4 edits across 2 sessions) leads, then c.go (2, one session); b.go is
	// edited once and never appears.
	all, err := st.FileChurn(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("file churn (all): %v", err)
	}
	if len(all.Files) != 2 {
		t.Fatalf("churn files = %d, want 2 (a.go, c.go); b.go and the no-path edit excluded: %+v", len(all.Files), all.Files)
	}
	if all.Files[0].Path != "a.go" || all.Files[1].Path != "c.go" {
		t.Errorf("churn order = %q, %q, want a.go, c.go", all.Files[0].Path, all.Files[1].Path)
	}
	if a := churnByPath(t, all.Files, "a.go"); a.Edits != 4 || a.Sessions != 2 {
		t.Errorf("a.go = {edits %d, sessions %d}, want {4, 2} (the replay collapsed)", a.Edits, a.Sessions)
	}
	if c := churnByPath(t, all.Files, "c.go"); c.Edits != 2 || c.Sessions != 1 {
		t.Errorf("c.go = {edits %d, sessions %d}, want {2, 1}", c.Edits, c.Sessions)
	}

	// Ada only: a.go drops to its three in-session edits; c.go (Grace's) drops out.
	ada1, err := st.FileChurn(ctx, store.AnalyticsFilter{Username: "ada"})
	if err != nil {
		t.Fatalf("file churn (ada): %v", err)
	}
	if len(ada1.Files) != 1 || ada1.Files[0].Path != "a.go" || ada1.Files[0].Edits != 3 || ada1.Files[0].Sessions != 1 {
		t.Errorf("ada churn = %+v, want only a.go {edits 3, sessions 1}", ada1.Files)
	}

	// A trailing window drops Grace's old session, so only Ada's a.go remains.
	windowed, err := st.FileChurn(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("file churn (windowed): %v", err)
	}
	if len(windowed.Files) != 1 || windowed.Files[0].Path != "a.go" || windowed.Files[0].Edits != 3 {
		t.Errorf("windowed churn = %+v, want only a.go with 3 edits", windowed.Files)
	}
}

// setSessionCwd sets a session's announced working directory, so a churn test can place two
// sessions in different git worktrees of one repo and confirm their edits collapse on the
// worktree-invariant relative path.
func setSessionCwd(t *testing.T, st *store.Store, ctx context.Context, sid int64, cwd string) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET cwd = $2 WHERE id = $1`, sid, cwd); err != nil {
		t.Fatalf("set cwd for %d: %v", sid, err)
	}
}

// TestFileChurnCollapsesWorktrees is the reason file_rel_path exists: two sessions in the SAME
// project but different working directories (two git worktrees of one repo) edit the same
// repo-relative file with different absolute paths, and the churn aggregate must fold them into a
// single row keyed on the relative path with Sessions = 2, not two per-worktree rows. The
// projection derives file_rel_path from each session's cwd at apply time, so the absolute paths
// differ while the stored relative path matches.
func TestFileChurnCollapsesWorktrees(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-1 * time.Hour)

	// Worktree A at C:\proj\akari edits internal\x.go twice.
	sA := seedSession(t, st, ada, pid, "wtA")
	setSessionCwd(t, st, ctx, sA, `C:\proj\akari`)
	if err := st.ApplyProjectionDelta(ctx, sA, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit"},
			{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: `C:\proj\akari\internal\x.go`, InputBody: "a1", CallUID: "a1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: `C:\proj\akari\internal\x.go`, InputBody: "a2", CallUID: "a2"},
		},
		ToolResults: []store.ToolResultDelta{{CallUID: "a1", Status: "ok"}, {CallUID: "a2", Status: "ok"}},
	}); err != nil {
		t.Fatalf("apply sA: %v", err)
	}
	setSessionShape(t, st, ctx, sA, recent, recent.Add(5*time.Minute), 2, 1)

	// Worktree B at C:\worktrees\akari\foo (differing drive-letter case too) edits the same
	// repo-relative file twice, from a different absolute path.
	sB := seedSession(t, st, ada, pid, "wtB")
	setSessionCwd(t, st, ctx, sB, `C:\worktrees\akari\foo`)
	if err := st.ApplyProjectionDelta(ctx, sB, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit"},
			{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: `c:\worktrees\akari\foo\internal\x.go`, InputBody: "b1", CallUID: "b1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: `c:\worktrees\akari\foo\internal\x.go`, InputBody: "b2", CallUID: "b2"},
		},
		ToolResults: []store.ToolResultDelta{{CallUID: "b1", Status: "ok"}, {CallUID: "b2", Status: "ok"}},
	}); err != nil {
		t.Fatalf("apply sB: %v", err)
	}
	setSessionShape(t, st, ctx, sB, recent, recent.Add(5*time.Minute), 2, 1)

	fc, err := st.FileChurn(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("file churn: %v", err)
	}
	if len(fc.Files) != 1 {
		t.Fatalf("churn files = %d, want 1 (both worktrees collapse on internal/x.go): %+v", len(fc.Files), fc.Files)
	}
	f := fc.Files[0]
	if f.Path != "internal/x.go" {
		t.Errorf("churn path = %q, want the relative internal/x.go", f.Path)
	}
	if f.Edits != 4 || f.Sessions != 2 {
		t.Errorf("churn = {edits %d, sessions %d}, want {4, 2} (collapsed across worktrees)", f.Edits, f.Sessions)
	}
	if f.Project != "github.com/jssblck/akari" {
		t.Errorf("churn project = %q, want the remote_key label github.com/jssblck/akari", f.Project)
	}
}

// TestFileChurnKeepsProjectsDistinct confirms the same relative path in two DIFFERENT projects
// stays two rows: the grouping key is (project, relative path), so a file named identically in two
// repos does not merge into a single misleading total.
func TestFileChurnKeepsProjectsDistinct(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	p1, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-1 * time.Hour)

	edit := func(sid int64, cwd, path, prefix string) {
		setSessionCwd(t, st, ctx, sid, cwd)
		if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
			Messages: []store.MessageDelta{
				{Ordinal: 0, Role: "user", Content: "edit"},
				{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
			},
			ToolCalls: []store.ProjToolCall{
				{MessageOrdinal: 1, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: path, InputBody: prefix + "1", CallUID: prefix + "1"},
				{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: path, InputBody: prefix + "2", CallUID: prefix + "2"},
			},
			ToolResults: []store.ToolResultDelta{{CallUID: prefix + "1", Status: "ok"}, {CallUID: prefix + "2", Status: "ok"}},
		}); err != nil {
			t.Fatalf("apply %s: %v", prefix, err)
		}
		setSessionShape(t, st, ctx, sid, recent, recent.Add(5*time.Minute), 2, 1)
	}

	s1 := seedSession(t, st, ada, p1, "proj1")
	edit(s1, "/home/ada/akari", "/home/ada/akari/README.md", "one")
	s2 := seedSession(t, st, ada, p2, "proj2")
	edit(s2, "/home/ada/engine", "/home/ada/engine/README.md", "two")

	fc, err := st.FileChurn(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("file churn: %v", err)
	}
	if len(fc.Files) != 2 {
		t.Fatalf("churn files = %d, want 2 (README.md in two projects stays distinct): %+v", len(fc.Files), fc.Files)
	}
	for _, f := range fc.Files {
		if f.Path != "README.md" {
			t.Errorf("churn path = %q, want README.md", f.Path)
		}
		if f.Edits != 2 || f.Sessions != 1 {
			t.Errorf("churn %+v, want {edits 2, sessions 1}", f)
		}
	}
	labels := map[string]bool{fc.Files[0].Project: true, fc.Files[1].Project: true}
	if !labels["github.com/jssblck/akari"] || !labels["github.com/ada/engine"] {
		t.Errorf("churn projects = %v, want both akari and engine remote_key labels", labels)
	}
}

// TestFileChurnClips confirms the list caps at maxChurnFiles with the overflow in Clipped.
func TestFileChurnClips(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	const distinct = 13
	var calls []store.ProjToolCall
	var results []store.ToolResultDelta
	idx := 0
	for i := 0; i < distinct; i++ {
		path := fmt.Sprintf("file%02d.go", i)
		for j := 0; j < 2; j++ { // two edits each -> every file churns
			uid := fmt.Sprintf("c%d-%d", i, j)
			calls = append(calls, store.ProjToolCall{
				MessageOrdinal: 1, CallIndex: idx, ToolName: "Edit", Category: "edit",
				FilePath: path, InputBody: uid, CallUID: uid,
			})
			results = append(results, store.ToolResultDelta{CallUID: uid, Status: "ok"})
			idx++
		}
	}
	sid := seedSession(t, st, u, pid, "clip")
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "edit"},
			{Ordinal: 1, Role: "assistant", Content: "editing", HasToolUse: true},
		},
		ToolCalls:   calls,
		ToolResults: results,
	}); err != nil {
		t.Fatalf("apply clip session: %v", err)
	}

	fc, err := st.FileChurn(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("file churn: %v", err)
	}
	if len(fc.Files) != 10 {
		t.Errorf("shown churn files = %d, want 10 (the list is capped)", len(fc.Files))
	}
	if fc.Clipped != distinct-10 {
		t.Errorf("Clipped = %d, want %d", fc.Clipped, distinct-10)
	}
}
