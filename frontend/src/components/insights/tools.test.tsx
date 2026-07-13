import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { Insights, Trends } from "../../types";
import { ToolsInstrument } from "./tools";
import { TooltipHost } from "./tooltip";

// A two-bucket Trends with every per-bucket array aligned, just enough for the
// Tools instrument's charts (and their useChartTooltip hover surfaces) to
// mount. The point of these tests is the provider contract: ToolsInstrument is
// rendered standalone on the project and public pages, outside InsightsPage's
// TooltipHost, and a missing provider throws at render time.
function trends(): Trends {
  return {
    Unit: "day",
    BucketStarts: ["2026-07-01T00:00:00Z", "2026-07-02T00:00:00Z"],
    Labels: ["Jul 1", "Jul 2"],
    FleetMix: {
      Models: [
        { Model: "fable-5", Share: [60, 70], First: 0, WindowShare: 65 },
      ],
      NewestModel: "fable-5",
      NewestFirst: 0,
    },
    Gallery: {
      Rows: [],
      Total: 0,
      MedianDurationS: 0,
      MedianCostUSD: 0,
      MedianCompletedCostUSD: 0,
      PriciestDurationS: 0,
      PriciestCostUSD: 0,
      LongestDurationS: 0,
      LongestCostUSD: 0,
      CostIncomplete: false,
    },
    Velocity: {
      ActiveHours: [1, 2],
      WallHours: [2, 3],
      ResponseP50: [4, 5],
      ResponseP90: [8, 9],
      ResponseP99: [20, 21],
      MsgsPerMin: [1, 1.5],
      ToolsPerMin: [2, 2.5],
    },
    Tools: {
      Reliability: [
        { Name: "Edit", Calls: 40, Failures: 2, Sessions: 5, Category: "edit" },
        { Name: "Bash", Calls: 30, Failures: 6, Sessions: 4, Category: "run" },
      ],
      MixOrder: ["edit", "run"],
      Mix: [
        { edit: 60, run: 40 },
        { edit: 55, run: 45 },
      ],
      FailFleet: [4, 6],
      FailWorst: [{ Name: "Bash", Rate: [10, 20] }],
    },
    Churn: {
      ReEdits: [3, 5],
      Files: [2, 4],
      Tree: [
        {
          Project: "github.com/adalovelace/engine",
          Folder: "notes",
          Path: "notes/program.md",
          Edits: 7,
          Sessions: 2,
        },
      ],
      Clipped: 0,
      TotalReEdits: 8,
      TotalHotFiles: 6,
      Projects: 1,
    },
    Signals: {
      GradeShare: [
        { A: 50, B: 50 },
        { A: 60, B: 40 },
      ],
      GPA: [3.5, 3.6],
      ArchetypeShare: [{ quick: 100 }, { quick: 100 }],
      CompletedRate: [80, 85],
      AbandonedRate: [10, 5],
      OutcomeTotal: [10, 12],
      CompletedCount: [8, 10],
      AbandonedCount: [1, 1],
      HygieneTerse: [1, 0],
      HygieneRepeated: [0, 0],
      HygieneNoCode: [2, 1],
      HygieneUnstructured: [0, 1],
      ContextResets: [0, 1],
      ContextHistogram: [{ Lo: 0, Hi: 100000, Count: 5 }],
      ContextMarkers: [{ Tokens: 80000, Kind: "p50" }],
    },
    Economics: {
      CostCompleted: [5, 6],
      CostAbandoned: [1, 0.5],
      CostOther: [0.2, 0.3],
      CacheSavings: [2, 2.5],
      CacheHitRate: [70, 75],
      CacheMeasured: [true, true],
      TotalSpend: 13,
      TotalAbandoned: 1.5,
      AbandonedSharePct: 11,
      TotalCacheSavings: 4.5,
      CacheHitRateLatest: 75,
      CostIncomplete: false,
      AbandonedIncomplete: false,
      CacheSavingsIncomplete: false,
    },
    Subagents: {
      DelegateShare: [10, 20],
      CostShare: [5, 15],
      FanoutOrder: ["one", "twoThree", "fourSeven", "eightPlus"],
      FanoutRows: [{ one: 100 }, { one: 100 }],
      SessionsThatDelegatePct: 15,
      SubagentSessionsInWindow: 3,
      CostThroughSubagentsPct: 10,
      DeepestTree: 2,
      CostShareIncomplete: false,
    },
    Rhythm: { Cells: Array.from({ length: 7 }, () => Array(24).fill(0)) },
  };
}

function insights(withTrends: Trends | null): Insights {
  return {
    Quality: {
      Grades: [{ Key: "A", Count: 3 }],
      Outcomes: [{ Key: "completed", Count: 3 }],
      Sessions: 3,
      Graded: 3,
    },
    Archetypes: [{ Key: "quick", Count: 3 }],
    Concurrency: {
      FleetPeak: 2,
      FleetPeakAt: "2026-07-01T12:00:00Z",
      BusiestUser: "grace",
      BusiestUserPeak: 2,
      AvgConcurrent: 1.2,
      Sessions: 3,
    },
    Velocity: {
      ResponseP50: 4e9,
      ResponseP90: 9e9,
      FirstResponseP50: 3e9,
      MsgsPerActiveMin: 1.5,
      ToolsPerActiveMin: 3,
      ActiveSeconds: 3600,
      Turns: 40,
      Sessions: 3,
    },
    Tools: {
      TotalCalls: 70,
      TotalFailures: 8,
      Turns: 40,
      Tools: [{ Name: "Edit", Calls: 40, Failures: 2 }],
      Clipped: 0,
    },
    Hygiene: {
      Prompts: 20,
      Short: 2,
      Duplicate: 1,
      NoCodeContext: 3,
      Sessions: 3,
      UnstructuredStarts: 1,
    },
    Churn: {
      Files: [
        {
          ProjectID: 1,
          Project: "github.com/adalovelace/engine",
          Path: "notes/program.md",
          Edits: 7,
          Sessions: 2,
        },
      ],
      Clipped: 0,
    },
    Context: {
      Sessions: 3,
      PeakTokensP50: 80000,
      PeakTokensP90: 150000,
      PeakTokensMax: 190000,
      TotalResets: 1,
      SessionsWithReset: 1,
    },
    Trends: withTrends,
  };
}

describe("ToolsInstrument standalone mounts", () => {
  it("renders inside a TooltipHost, as the project and public pages mount it", () => {
    render(
      <TooltipHost>
        <ToolsInstrument insights={insights(trends())} resetKey="6:30d" />
      </TooltipHost>,
    );
    expect(screen.getByRole("heading", { name: "Tools" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Churn" })).toBeInTheDocument();
  });

  it("throws without a TooltipHost, so standalone call sites must provide one", () => {
    // The provider-less mount is the exact crash the project detail page
    // shipped with; keep it loud so nobody mounts an instrument bare again.
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    try {
      expect(() =>
        render(
          <ToolsInstrument insights={insights(trends())} resetKey="6:30d" />,
        ),
      ).toThrow(/TooltipHost/);
    } finally {
      consoleError.mockRestore();
    }
  });

  it("renders nothing when Trends is null", () => {
    const { container } = render(
      <ToolsInstrument insights={insights(null)} resetKey="6:30d" />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
