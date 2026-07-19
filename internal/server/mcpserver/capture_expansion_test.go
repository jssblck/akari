package mcpserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func TestCaptureExpansionDTOs(t *testing.T) {
	detail := sessionDetailToDTO(store.SessionDetail{
		Slug:            "quiet-circuit",
		PermissionMode:  "bypassPermissions",
		ReasoningEffort: "high",
		SubagentName:    "Explore",
		PRNumber:        42,
		PRURL:           "https://github.com/ada/engine/pull/42",
		PRRepo:          "ada/engine",
	})
	if detail.Slug != "quiet-circuit" || detail.PermissionMode != "bypassPermissions" || detail.ReasoningEffort != "high" || detail.SubagentName != "Explore" || detail.PRNumber != 42 || detail.PRRepo != "ada/engine" {
		t.Fatalf("identity DTO = %+v", detail)
	}

	tool := toolCallToDTO(store.ToolCallView{
		StructSHA256:      "abc",
		StructBytes:       17,
		StructMediaType:   "application/json",
		AttributionAgent:  "Explore",
		AttributionSkill:  "review",
		AttributionPlugin: "github",
	})
	if tool.StructSHA256 != "abc" || tool.StructBytes != 17 || tool.StructMediaType != "application/json" || tool.AttributionAgent != "Explore" || tool.AttributionSkill != "review" || tool.AttributionPlugin != "github" {
		t.Fatalf("tool DTO = %+v", tool)
	}

	ordinal := int64(7)
	occurred := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	event := sessionEventToDTO(store.SessionEvent{
		MessageOrdinal: &ordinal,
		Kind:           "api_error",
		Attrs:          json.RawMessage(`{"message":"retrying"}`),
		OccurredAt:     occurred,
	})
	if event.MessageOrdinal == nil || *event.MessageOrdinal != ordinal || event.Kind != "api_error" || string(event.Attrs) != `{"message":"retrying"}` || !event.OccurredAt.Equal(occurred) {
		t.Fatalf("event DTO = %+v", event)
	}
}
