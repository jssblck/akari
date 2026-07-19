import { describe, expect, it } from "vitest";

import {
  baseName,
  contextLabel,
  contextStamp,
  detailLabel,
  fallbackBadgeLabel,
  fallbackBadgeTitle,
  fallbackCategoryLabel,
  fallbackDeclinedObserved,
  fallbackModelsLabel,
  fallbackNoticeLabel,
  formatDurationPositive,
  formatDurationSpan,
  formatLatency,
  gradeBand,
  hasHygieneSignal,
  isContextReset,
  isDiffTool,
  isScored,
  outcomeLabel,
  outlineTitle,
  qualityGrade,
  qualityScoreLabel,
  rowOutcomeNote,
  scoreBreakdownItems,
  shedLabel,
  stripPromptPreamble,
  thinkingBucketForTokens,
  thinkingBucketLabel,
  thinkingBytesPerTokenFor,
  thinkingTokensLabel,
  toolFilePath,
  toolFileTitle,
  turnCostLabel,
  turnLatency,
  turnTokenTotal,
} from "./session-quality";
import type {
  ModelFallback,
  SessionSignals,
  TurnUsageFull,
} from "./session-types";

function signals(overrides: Partial<SessionSignals> = {}): SessionSignals {
  return {
    SessionID: 1,
    Outcome: "completed",
    OutcomeConfidence: "high",
    Score: null,
    Grade: null,
    ToolCalls: 0,
    ToolFailures: 0,
    ToolRetries: 0,
    EditChurn: 0,
    LongestFailureStreak: 0,
    PromptCount: 0,
    ShortPromptCount: 0,
    DuplicatePromptCount: 0,
    NoCodeContextCount: 0,
    UnstructuredStart: false,
    PeakContextTokens: null,
    ContextResetCount: null,
    AssistantTurns: null,
    ThinkingTurns: null,
    ThinkingTailTokens: null,
    ThinkingPeakTokens: null,
    ...overrides,
  };
}

describe("gradeBand", () => {
  it("bands A and B as good", () => {
    expect(gradeBand("A")).toBe("good");
    expect(gradeBand("B")).toBe("good");
  });
  it("bands C as watch", () => {
    expect(gradeBand("C")).toBe("watch");
  });
  it("bands D and F as poor", () => {
    expect(gradeBand("D")).toBe("poor");
    expect(gradeBand("F")).toBe("poor");
  });
  it("bands null as none", () => {
    expect(gradeBand(null)).toBe("none");
  });
});

describe("quality grade / score label", () => {
  it("is unscored when Score and Grade are both null", () => {
    const s = signals();
    expect(isScored(s)).toBe(false);
    expect(qualityGrade(s)).toBe("-");
    expect(qualityScoreLabel(s)).toBe("not scored");
  });

  it("reads the letter and the score once both are set", () => {
    const s = signals({ Score: 87, Grade: "B" });
    expect(isScored(s)).toBe(true);
    expect(qualityGrade(s)).toBe("B");
    expect(qualityScoreLabel(s)).toBe("87 / 100");
  });
});

describe("outcomeLabel / rowOutcomeNote", () => {
  it("labels every known outcome", () => {
    expect(outcomeLabel("completed")).toBe("Completed");
    expect(outcomeLabel("abandoned")).toBe("Abandoned");
    expect(outcomeLabel("errored")).toBe("Errored");
    expect(outcomeLabel("unknown")).toBe("Unknown");
    expect(outcomeLabel("something-else")).toBe("Unknown");
  });

  it("only flags abandoned and errored on the feed row", () => {
    expect(rowOutcomeNote("abandoned")).toBe("abandoned");
    expect(rowOutcomeNote("errored")).toBe("errored");
    expect(rowOutcomeNote("completed")).toBe("");
    expect(rowOutcomeNote("unknown")).toBe("");
  });
});

describe("scoreBreakdownItems", () => {
  it("is empty for an unknown outcome with no tool signal", () => {
    expect(scoreBreakdownItems(signals({ Outcome: "unknown" }))).toEqual([]);
  });

  it("lists the errored penalty", () => {
    expect(scoreBreakdownItems(signals({ Outcome: "errored" }))).toEqual([
      { label: "errored ending", points: 30 },
    ]);
  });

  it("lists the abandoned penalty", () => {
    expect(scoreBreakdownItems(signals({ Outcome: "abandoned" }))).toEqual([
      { label: "abandoned", points: 15 },
    ]);
  });

  it("caps per-failure penalties and pluralizes the label", () => {
    // 15 failures * 3 pts/failure = 45, capped at 30.
    const items = scoreBreakdownItems(
      signals({ Outcome: "unknown", ToolFailures: 15 }),
    );
    expect(items).toEqual([{ label: "15 tool failures", points: 30 }]);
  });

  it("singularizes a single-count label", () => {
    const items = scoreBreakdownItems(
      signals({ Outcome: "unknown", ToolFailures: 1 }),
    );
    expect(items).toEqual([{ label: "1 tool failure", points: 3 }]);
  });

  it("adds the failure-streak penalty once the streak floor is reached", () => {
    const items = scoreBreakdownItems(
      signals({ Outcome: "unknown", LongestFailureStreak: 3 }),
    );
    expect(items).toEqual([{ label: "failure streak", points: 10 }]);
  });

  it("stacks every penalty in order for a rough session", () => {
    const items = scoreBreakdownItems(
      signals({
        Outcome: "errored",
        ToolFailures: 2,
        ToolRetries: 3,
        EditChurn: 1,
        LongestFailureStreak: 4,
      }),
    );
    expect(items).toEqual([
      { label: "errored ending", points: 30 },
      { label: "2 tool failures", points: 6 },
      { label: "3 retries", points: 15 },
      { label: "1 churned edit", points: 4 },
      { label: "failure streak", points: 10 },
    ]);
  });
});

describe("hasHygieneSignal", () => {
  it("is false when every hygiene count is clean", () => {
    expect(hasHygieneSignal(signals())).toBe(false);
  });
  it("is true when any hygiene count is nonzero", () => {
    expect(hasHygieneSignal(signals({ ShortPromptCount: 1 }))).toBe(true);
    expect(hasHygieneSignal(signals({ UnstructuredStart: true }))).toBe(true);
  });
});

describe("thinking bucket helpers", () => {
  it("bands tokens at the low/medium/high boundaries", () => {
    expect(thinkingBucketForTokens(0)).toBe("low");
    expect(thinkingBucketForTokens(128)).toBe("low");
    expect(thinkingBucketForTokens(129)).toBe("medium");
    expect(thinkingBucketForTokens(512)).toBe("medium");
    expect(thinkingBucketForTokens(513)).toBe("high");
    expect(thinkingBucketForTokens(2048)).toBe("high");
    expect(thinkingBucketForTokens(2049)).toBe("xhigh");
  });

  it("labels the top band as 'very high' and passes the rest through", () => {
    expect(thinkingBucketLabel("xhigh")).toBe("very high");
    expect(thinkingBucketLabel("low")).toBe("low");
    expect(thinkingBucketLabel("medium")).toBe("medium");
    expect(thinkingBucketLabel("high")).toBe("high");
  });

  it("formats an approximate token count", () => {
    expect(thinkingTokensLabel(1500)).toBe("~1.5k tok");
  });

  it("looks up the agent-specific bytes-per-token divisor, defaulting to Claude's", () => {
    expect(thinkingBytesPerTokenFor("claude")).toBeCloseTo(10.7);
    expect(thinkingBytesPerTokenFor("codex")).toBeCloseTo(14.2);
    expect(thinkingBytesPerTokenFor("pi")).toBeCloseTo(4.0);
    expect(thinkingBytesPerTokenFor("unknown-agent")).toBeCloseTo(10.7);
  });
});

describe("isContextReset", () => {
  it("fires when the prior turn was large and this turn dropped to half or less", () => {
    expect(isContextReset(20000, 10000)).toBe(true);
    expect(isContextReset(40000, 15000)).toBe(true);
  });
  it("does not fire when the prior turn was below the keep floor", () => {
    expect(isContextReset(19999, 100)).toBe(false);
  });
  it("does not fire when the drop is less than half", () => {
    expect(isContextReset(20000, 10001)).toBe(false);
  });
});

describe("turn usage / cost / context helpers", () => {
  function usage(overrides: Partial<TurnUsageFull> = {}): TurnUsageFull {
    return {
      Input: 100,
      Output: 50,
      CacheRead: 10,
      CacheWrite: 5,
      Reasoning: 0,
      CostUSD: 0.05,
      ContextTokens: 1234,
      ...overrides,
    };
  }

  it("sums the four token fields", () => {
    expect(turnTokenTotal(usage())).toBe(165);
  });

  it("formats an unknown price as zero", () => {
    const label = turnCostLabel(usage({ CostUSD: 0 }), (v) => `$${v}`);
    expect(label).toBe("$0");
  });

  it("formats a priced turn through the given formatter", () => {
    const label = turnCostLabel(usage({ CostUSD: 0.5 }), (v) => `$${v}`);
    expect(label).toBe("$0.5");
  });

  it("stamps the context token figure", () => {
    expect(contextStamp(usage({ ContextTokens: 12300 }))).toBe("ctx 12.3k");
  });

  it("labels a context shed from/to", () => {
    expect(shedLabel(50000, 8000)).toBe("context shed: 50.0k → 8.0k");
  });
});

describe("formatLatency / turnLatency", () => {
  it("reads a dash for zero or negative durations", () => {
    expect(formatLatency(0)).toBe("-");
    expect(formatLatency(-5)).toBe("-");
  });
  it("reads sub-second latency as <1s", () => {
    expect(formatLatency(400)).toBe("<1s");
  });
  it("rounds whole seconds under a minute", () => {
    expect(formatLatency(45_000)).toBe("45s");
  });
  it("formats minutes and seconds under an hour", () => {
    expect(formatLatency(125_000)).toBe("2m 5s");
  });
  it("formats hours and minutes at an hour or above", () => {
    expect(formatLatency(3_725_000)).toBe("1h 2m");
  });
  it("prefixes a turn latency with +", () => {
    expect(turnLatency(45_000)).toBe("+45s");
  });
});

describe("formatDurationPositive / formatDurationSpan", () => {
  it("formats seconds only under a minute", () => {
    expect(formatDurationPositive(45_000)).toBe("45s");
  });
  it("formats minutes and seconds under an hour", () => {
    expect(formatDurationPositive(125_000)).toBe("2m5s");
  });
  it("formats hours and minutes at an hour or above", () => {
    expect(formatDurationPositive(3_725_000)).toBe("1h2m");
  });
  it("spans a start and end timestamp", () => {
    expect(
      formatDurationSpan("2026-01-01T00:00:00Z", "2026-01-01T00:02:05Z"),
    ).toBe("2m5s");
  });
  it("reads a dash when either endpoint is missing", () => {
    expect(formatDurationSpan(null, "2026-01-01T00:00:00Z")).toBe("-");
    expect(formatDurationSpan("2026-01-01T00:00:00Z", null)).toBe("-");
  });
  it("reads a dash when the end precedes the start", () => {
    expect(
      formatDurationSpan("2026-01-01T00:02:00Z", "2026-01-01T00:00:00Z"),
    ).toBe("-");
  });
});

describe("fallback helpers", () => {
  function fallback(overrides: Partial<ModelFallback> = {}): ModelFallback {
    return {
      MessageOrdinal: 4,
      FromModel: "sonnet",
      ToModel: "haiku",
      Trigger: "context_limit",
      RefusalCategory: "",
      RefusalExplanation: "",
      DeclinedInput: null,
      DeclinedOutput: null,
      DeclinedCacheWrite: null,
      DeclinedCacheRead: null,
      OccurredAt: null,
      DedupKey: "x",
      ...overrides,
    };
  }

  it("labels a single fallback without a count suffix", () => {
    expect(fallbackBadgeLabel(1)).toBe("fallback");
    expect(fallbackBadgeTitle(1)).toBe(
      "1 turn fell back from Fable 5 to a lower model",
    );
  });

  it("labels multiple fallbacks with a count suffix", () => {
    expect(fallbackBadgeLabel(3)).toBe("fallback ×3");
    expect(fallbackBadgeTitle(3)).toBe(
      "3 turns fell back from Fable 5 to a lower model",
    );
  });

  it("formats the from/to model pair, defaulting an empty model to 'unknown'", () => {
    expect(fallbackModelsLabel(fallback())).toBe("sonnet → haiku");
    expect(fallbackModelsLabel(fallback({ FromModel: "" }))).toBe(
      "unknown → haiku",
    );
  });

  it("falls back to 'uncategorized' when no refusal category is set", () => {
    expect(
      fallbackCategoryLabel(fallback({ RefusalCategory: "refusal" })),
    ).toBe("refusal");
    expect(fallbackCategoryLabel(fallback())).toBe("uncategorized");
  });

  it("only reports declined usage observed once all four fields are present", () => {
    expect(fallbackDeclinedObserved(fallback())).toBe(false);
    expect(
      fallbackDeclinedObserved(
        fallback({
          DeclinedInput: 1,
          DeclinedOutput: 1,
          DeclinedCacheWrite: 0,
          DeclinedCacheRead: 0,
        }),
      ),
    ).toBe(true);
  });

  it("prefers the refusal category over the raw trigger in the notice label", () => {
    expect(fallbackNoticeLabel(fallback({ RefusalCategory: "overload" }))).toBe(
      "Fell back from sonnet to haiku (overload)",
    );
    expect(fallbackNoticeLabel(fallback())).toBe(
      "Fell back from sonnet to haiku (context_limit)",
    );
  });
});

describe("contextLabel", () => {
  it("names project instructions plus environment when both markers are present", () => {
    const content =
      "# AGENTS.md instructions for repo\n<environment_context>x</environment_context>";
    expect(contextLabel(content)).toBe("project instructions + environment");
  });
  it("names project instructions alone", () => {
    expect(contextLabel("# AGENTS.md instructions for repo")).toBe(
      "project instructions",
    );
  });
  it("names environment alone", () => {
    expect(contextLabel("<environment_context>x</environment_context>")).toBe(
      "environment",
    );
  });
  it("falls back to a generic label", () => {
    expect(contextLabel("something else entirely")).toBe("agent context");
  });
  it("labels system turns without inspecting their contents", () => {
    expect(contextLabel("policy text", "system")).toBe("system prompt");
  });
});

describe("isDiffTool", () => {
  it("recognizes every known editing tool, case-insensitively", () => {
    expect(isDiffTool("Edit")).toBe(true);
    expect(isDiffTool("write")).toBe(true);
    expect(isDiffTool("APPLY_PATCH")).toBe(true);
  });
  it("rejects a non-editing tool", () => {
    expect(isDiffTool("bash")).toBe(false);
  });
});

describe("outlineTitle", () => {
  it("labels a context turn through contextLabel", () => {
    expect(
      outlineTitle("context", "<environment_context>x</environment_context>"),
    ).toBe("environment");
  });
  it("labels a system turn through contextLabel", () => {
    expect(outlineTitle("system", "policy text")).toBe("system prompt");
  });
  it("collapses whitespace and passes short content through unchanged", () => {
    expect(outlineTitle("user", "  fix the   bug  ")).toBe("fix the bug");
  });
  it("truncates long content at 48 characters with an ellipsis", () => {
    const long = "a".repeat(60);
    const title = outlineTitle("assistant", long);
    expect(title).toBe(`${"a".repeat(48)}…`);
  });
});

describe("detailLabel", () => {
  it("passes short text through unchanged", () => {
    expect(detailLabel("ls -la")).toBe("ls -la");
  });
  it("truncates at 80 characters with an ellipsis", () => {
    const long = "b".repeat(100);
    expect(detailLabel(long)).toBe(`${"b".repeat(80)}…`);
  });
});

describe("baseName", () => {
  it("takes the last segment of a forward-slash path", () => {
    expect(baseName("src/components/session-tags.tsx")).toBe(
      "session-tags.tsx",
    );
  });
  it("takes the last segment of a backslash path", () => {
    expect(baseName("C:\\Users\\ada\\lovelace.py")).toBe("lovelace.py");
  });
  it("returns the whole string when there is no separator", () => {
    expect(baseName("lovelace.py")).toBe("lovelace.py");
  });
});

describe("toolFilePath / toolFileTitle", () => {
  it("prefers the worktree-relative path for display", () => {
    expect(
      toolFilePath({ FileRelPath: "src/a.ts", FilePath: "/repo/src/a.ts" }),
    ).toBe("src/a.ts");
  });
  it("falls back to the absolute path when no relative path is known", () => {
    expect(toolFilePath({ FileRelPath: "", FilePath: "/repo/src/a.ts" })).toBe(
      "/repo/src/a.ts",
    );
  });
  it("surfaces the absolute path as a title only when it differs", () => {
    expect(
      toolFileTitle({ FileRelPath: "src/a.ts", FilePath: "/repo/src/a.ts" }),
    ).toBe("/repo/src/a.ts");
    expect(toolFileTitle({ FileRelPath: "", FilePath: "/repo/src/a.ts" })).toBe(
      "",
    );
    expect(
      toolFileTitle({ FileRelPath: "src/a.ts", FilePath: "src/a.ts" }),
    ).toBe("");
  });
});

describe("stripPromptPreamble", () => {
  it("names a slash command with its arguments", () => {
    const t =
      "<command-message>run</command-message><command-name>/deploy</command-name><command-args>prod</command-args>";
    expect(stripPromptPreamble(t)).toBe("/deploy prod");
  });
  it("names a bare slash command with no arguments", () => {
    const t =
      "<command-message>run</command-message><command-name>/status</command-name><command-args></command-args>";
    expect(stripPromptPreamble(t)).toBe("/status");
  });
  it("strips a leading system-reminder wrapper", () => {
    const t = "<system-reminder>heads up</system-reminder>the real prompt";
    expect(stripPromptPreamble(t)).toBe("the real prompt");
  });
  it("strips an AGENTS.md instructions block up to its closing tag", () => {
    const t =
      "# AGENTS.md instructions for repo\nsome rules</INSTRUCTIONS>the real prompt";
    expect(stripPromptPreamble(t)).toBe("the real prompt");
  });
  it("returns the original title when stripping would leave nothing", () => {
    expect(stripPromptPreamble("   ")).toBe("   ");
  });
});
