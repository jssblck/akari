package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	serverparse "github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestRebuildStoresAndReplacesEventsAndIdentity(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "capture-parent", ProjectID: pid}); err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "capture-parent/subagents/child", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}

	ordinal := 4
	occurred := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{
		Events: []store.EventDelta{
			{MessageOrdinal: &ordinal, Kind: "turn_end", AttrsJSON: `{"duration_ms":1234}`, OccurredAt: occurred},
			{Kind: "compaction", AttrsJSON: `{}`},
		},
		Identity: store.SessionIdentityDelta{
			CustomTitle: "Review auth", Slug: "quiet-circuit", PermissionMode: "bypassPermissions",
			ReasoningEffort: "high", SubagentName: "Explore", PRNumber: 42,
			PRURL: "https://github.com/ada/engine/pull/42", PRRepo: "ada/engine",
		},
	})

	var (
		gotOrdinal  *int
		gotKind     string
		gotDuration string
		gotOccurred time.Time
	)
	if err := st.Pool.QueryRow(ctx,
		`SELECT message_ordinal, kind, attrs->>'duration_ms', occurred_at
		   FROM session_events WHERE session_id=$1 AND seq=0`, ann.SessionID).
		Scan(&gotOrdinal, &gotKind, &gotDuration, &gotOccurred); err != nil {
		t.Fatal(err)
	}
	if gotOrdinal == nil || *gotOrdinal != ordinal || gotKind != "turn_end" || gotDuration != "1234" || !gotOccurred.Equal(occurred) {
		t.Fatalf("first event = ordinal %v kind %q duration %q at %v", gotOrdinal, gotKind, gotDuration, gotOccurred)
	}

	var customTitle, slug, permission, effort, subagent, prURL, prRepo, parentSource string
	var prNumber int
	if err := st.Pool.QueryRow(ctx,
		`SELECT custom_title, slug, permission_mode, reasoning_effort, subagent_name,
		        pr_number, pr_url, pr_repo, parent_source_id
		   FROM sessions WHERE id=$1`, ann.SessionID).
		Scan(&customTitle, &slug, &permission, &effort, &subagent, &prNumber, &prURL, &prRepo, &parentSource); err != nil {
		t.Fatal(err)
	}
	if customTitle != "Review auth" || slug != "quiet-circuit" || permission != "bypassPermissions" || effort != "high" || subagent != "Explore" || prNumber != 42 || prURL != "https://github.com/ada/engine/pull/42" || prRepo != "ada/engine" {
		t.Fatalf("stored identity = %q %q %q %q %q %d %q %q", customTitle, slug, permission, effort, subagent, prNumber, prURL, prRepo)
	}
	if parentSource != "capture-parent" {
		t.Fatalf("announce-derived parent_source_id = %q, want capture-parent", parentSource)
	}
	detail, err := st.SessionDetailByID(ctx, ann.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title != "Review auth" || detail.Slug != "quiet-circuit" || detail.PermissionMode != "bypassPermissions" || detail.ReasoningEffort != "high" || detail.SubagentName != "Explore" || detail.PRNumber != 42 || detail.PRURL != "https://github.com/ada/engine/pull/42" || detail.PRRepo != "ada/engine" {
		t.Fatalf("read identity = %+v", detail)
	}
	events, err := st.SessionEvents(ctx, ann.SessionID, store.ModelFallbackListCap)
	if err != nil {
		t.Fatal(err)
	}
	var eventAttrs map[string]int
	if len(events) > 0 {
		if err := json.Unmarshal(events[0].Attrs, &eventAttrs); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 2 || events[0].MessageOrdinal == nil || *events[0].MessageOrdinal != int64(ordinal) || events[0].Kind != "turn_end" || eventAttrs["duration_ms"] != 1234 || !events[0].OccurredAt.Equal(occurred) {
		t.Fatalf("read events = %+v", events)
	}

	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{
		Events:   []store.EventDelta{{Kind: "api_error", AttrsJSON: `{"retry_attempt":2}`}},
		Identity: store.SessionIdentityDelta{CustomTitle: "Second title"},
	})
	var eventCount int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM session_events WHERE session_id=$1", ann.SessionID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("replacement left %d events, want 1", eventCount)
	}
	if err := st.Pool.QueryRow(ctx,
		"SELECT custom_title, slug, parent_source_id FROM sessions WHERE id=$1", ann.SessionID).
		Scan(&customTitle, &slug, &parentSource); err != nil {
		t.Fatal(err)
	}
	if customTitle != "Second title" || slug != "" || parentSource != "capture-parent" {
		t.Fatalf("replacement identity = title %q slug %q parent %q", customTitle, slug, parentSource)
	}

	if err := st.ResetRaw(ctx, ann.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM session_events WHERE session_id=$1),
		        custom_title, parent_source_id
		   FROM sessions WHERE id=$1`, ann.SessionID).
		Scan(&eventCount, &customTitle, &parentSource); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || customTitle != "" || parentSource != "capture-parent" {
		t.Fatalf("reset state = events %d title %q parent %q", eventCount, customTitle, parentSource)
	}
}

func TestStructuredToolResultBlobsUseBothCASPaths(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/ada/compiler", "github.com", "ada", "compiler", "compiler", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "structured-results", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}

	inline := `{"files":["auth.go"]}`
	lifted := `{"tests":17}`
	inlineSHA := store.HashString(inline)
	liftedSHA := store.HashString(lifted)
	if err := st.PutBlob(ctx, liftedSHA, "application/json", "application/octet-stream", strings.NewReader(lifted)); err != nil {
		t.Fatal(err)
	}
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "inline", AttributionAgent: "Explore", AttributionSkill: "review", AttributionPlugin: "github"},
			{MessageOrdinal: 0, CallIndex: 1, ToolName: "Bash", CallUID: "lifted"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "inline", Status: "ok", StructBody: inline, StructBytes: len(inline), StructMediaType: "application/json"},
			{CallUID: "lifted", Status: "ok", StructSHA256: liftedSHA, StructBytes: len(lifted), StructMediaType: "application/json"},
		},
	}
	rebuildWith(t, st, ann.SessionID, delta)

	rows, err := st.Pool.Query(ctx,
		`SELECT call_index, struct_sha256, struct_bytes, struct_media_type,
		        attribution_agent, attribution_skill, attribution_plugin
		   FROM tool_calls WHERE session_id=$1 ORDER BY call_index`, ann.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantSHAs := []string{inlineSHA, liftedSHA}
	for i := 0; rows.Next(); i++ {
		var callIndex int
		var sha, media, agent, skill, plugin string
		var bytes int64
		if err := rows.Scan(&callIndex, &sha, &bytes, &media, &agent, &skill, &plugin); err != nil {
			t.Fatal(err)
		}
		if callIndex != i || sha != wantSHAs[i] || media != "application/json" {
			t.Fatalf("structured row %d = index %d sha %q media %q", i, callIndex, sha, media)
		}
		if i == 0 && (bytes != int64(len(inline)) || agent != "Explore" || skill != "review" || plugin != "github") {
			t.Fatalf("inline metadata = bytes %d attribution %q/%q/%q", bytes, agent, skill, plugin)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, sha := range wantSHAs {
		ok, err := st.SessionReferencesBlob(ctx, ann.SessionID, sha)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("session does not authorize structured blob %s", sha)
		}
	}
	tools, err := st.ToolCalls(ctx, ann.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].StructSHA256 != inlineSHA || tools[0].StructBytes != int64(len(inline)) || tools[0].StructMediaType != "application/json" || tools[0].AttributionAgent != "Explore" || tools[0].AttributionSkill != "review" || tools[0].AttributionPlugin != "github" || tools[1].StructSHA256 != liftedSHA {
		t.Fatalf("read structured tools = %+v", tools)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at=now()-interval '1 hour' WHERE sha256=ANY($1)", wantSHAs); err != nil {
		t.Fatal(err)
	}
	rebuildWith(t, st, ann.SessionID, delta)
	var pinned int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM blob_pins WHERE sha256=ANY($1) AND expires_at > now()", wantSHAs).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != len(wantSHAs) {
		t.Fatalf("rebuild pinned %d structured blobs, want %d", pinned, len(wantSHAs))
	}
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with live structured references removed=%d err=%v", removed, err)
	}
}

func TestCodexRebuildLinksSubagentsInBothOrders(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "anna")
	pid, err := st.UpsertProject(ctx, "github.com/anna/logic", "github.com", "anna", "logic", "logic", "remote")
	if err != nil {
		t.Fatal(err)
	}
	announce := func(source string) int64 {
		t.Helper()
		ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "codex", SourceSessionID: source, ProjectID: pid})
		if err != nil {
			t.Fatalf("announce %q: %v", source, err)
		}
		return ann.SessionID
	}
	rebuildChild := func(sessionID int64, parentSource, name string) {
		t.Helper()
		raw := []byte(`{"timestamp":"2026-07-18T12:00:00Z","type":"session_meta","payload":{"parent_thread_id":"` + parentSource + `","agent_path":"root/` + name + `"}}` + "\n")
		if _, err := st.AppendChunk(ctx, sessionID, 0, raw); err != nil {
			t.Fatalf("append child metadata: %v", err)
		}
		if err := serverparse.Rebuild(ctx, st, sessionID, "codex"); err != nil {
			t.Fatalf("rebuild child: %v", err)
		}
	}
	assertParent := func(child, parent int64) {
		t.Helper()
		var gotParent *int64
		var relationship, parentSource string
		if err := st.Pool.QueryRow(ctx,
			"SELECT parent_session_id, relationship_type, parent_source_id FROM sessions WHERE id=$1", child).
			Scan(&gotParent, &relationship, &parentSource); err != nil {
			t.Fatal(err)
		}
		if gotParent == nil || *gotParent != parent || relationship != "subagent" || parentSource == "" {
			t.Fatalf("child %d link = parent %v relationship %q source %q", child, gotParent, relationship, parentSource)
		}
	}

	childFirst := announce("codex-child-first")
	rebuildChild(childFirst, "codex-parent-late", "reviewer")
	var unlinked *int64
	if err := st.Pool.QueryRow(ctx, "SELECT parent_session_id FROM sessions WHERE id=$1", childFirst).Scan(&unlinked); err != nil {
		t.Fatal(err)
	}
	if unlinked != nil {
		t.Fatalf("child linked before parent existed: %d", *unlinked)
	}
	lateParent := announce("codex-parent-late")
	assertParent(childFirst, lateParent)

	parentFirst := announce("codex-parent-first")
	childAfter := announce("codex-child-after")
	rebuildChild(childAfter, "codex-parent-first", "tester")
	assertParent(childAfter, parentFirst)
	subagents, err := st.Subagents(ctx, parentFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(subagents) != 1 || subagents[0].ID != childAfter || subagents[0].SubagentName != "tester" {
		t.Fatalf("subagent read rows = %+v", subagents)
	}
}
