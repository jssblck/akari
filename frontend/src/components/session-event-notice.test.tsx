import { describe, expect, it } from "vitest";

import type { SessionEvent } from "../types";
import { sessionEventLabel } from "./session-event-notice";

function event(kind: string, attrs: SessionEvent["Attrs"] = {}): SessionEvent {
  return {
    Attrs: attrs,
    Kind: kind,
    MessageOrdinal: 3,
    OccurredAt: "2026-07-18T12:00:00Z",
  };
}

describe("sessionEventLabel", () => {
  it("renders compact Codex and detailed Claude compactions", () => {
    expect(sessionEventLabel(event("compaction"))).toBe("context compacted");
    expect(
      sessionEventLabel(
        event("compaction", {
          trigger: "auto",
          pre_tokens: 120000,
          post_tokens: 30000,
          dropped_tokens: 90000,
        }),
      ),
    ).toBe("context compacted / auto / 120.0k to 30.0k / 90.0k dropped");
  });

  it("renders interruption and pi changes", () => {
    expect(sessionEventLabel(event("turn_aborted", { reason: "user" }))).toBe(
      "turn aborted / user",
    );
    expect(
      sessionEventLabel(
        event("model_change", { provider: "openai", model: "gpt-5" }),
      ),
    ).toBe("model changed / openai/gpt-5");
    expect(
      sessionEventLabel(event("thinking_level_change", { level: "high" })),
    ).toBe("thinking level changed / high");
  });

  it("only shows actionable stop hooks", () => {
    expect(sessionEventLabel(event("stop_hook", { hook_count: 1 }))).toBeNull();
    expect(
      sessionEventLabel(
        event("stop_hook", {
          prevented_continuation: true,
          errors: ["lint failed"],
        }),
      ),
    ).toBe("stop hook prevented continuation / lint failed");
  });

  it("hides turn telemetry and subagent activity", () => {
    expect(sessionEventLabel(event("turn_end"))).toBeNull();
    expect(sessionEventLabel(event("subagent_activity"))).toBeNull();
  });
});
