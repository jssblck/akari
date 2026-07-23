import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { Trends } from "../../types";
import { FleetMixInstrument } from "./fleet-mix";
import { TooltipHost } from "./tooltip";

// The chart's hover hit rect spans the full SVG viewBox (1000x380); jsdom
// never lays out real pixel boxes, so give it that box back to make
// clientX 0 land reliably on bucket index 0.
function hoverBucket(container: HTMLElement, clientX: number) {
  const hitRect = container.querySelector(
    'svg rect[fill="transparent"]',
  ) as SVGRectElement;
  hitRect.getBoundingClientRect = () =>
    ({
      left: 0,
      top: 0,
      width: 1000,
      height: 380,
      right: 1000,
      bottom: 380,
      x: 0,
      y: 0,
      toJSON() {},
    }) as DOMRect;
  fireEvent.mouseMove(hitRect, { clientX, clientY: 0 });
}

function trends(
  bucketStarts: string[],
  labels: string[],
  models = ["claude-sonnet-5", "claude-opus-4-8"],
): Trends {
  const n = bucketStarts.length;
  return {
    Unit: "week",
    BucketStarts: bucketStarts,
    Labels: labels,
    FleetMix: {
      Models: models.map((model, i) => ({
        Model: model,
        Share: Array(n).fill(i === 0 ? 70 : 30),
        First: 0,
        WindowShare: i === 0 ? 70 : 30,
      })),
      // Deliberately not one of the models above: the arrival chip and the
      // legend chips are matched by text, so a shared label would make
      // getByText ambiguous rather than assert anything.
      NewestModel: "claude-haiku-4-5-20251001",
      NewestFirst: n - 1,
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
    },
    Velocity: {
      ActiveHours: Array(n).fill(0),
      WallHours: Array(n).fill(0),
      ResponseP50: Array(n).fill(0),
      ResponseP90: Array(n).fill(0),
      ResponseP99: Array(n).fill(0),
      MsgsPerMin: Array(n).fill(0),
      ToolsPerMin: Array(n).fill(0),
    },
    Tools: {
      Reliability: [],
      MixOrder: [],
      Mix: Array(n).fill({}),
      FailFleet: Array(n).fill(0),
      FailWorst: [],
    },
    Churn: {
      ReEdits: Array(n).fill(0),
      Files: Array(n).fill(0),
      Tree: [],
      Clipped: 0,
      TotalReEdits: 0,
      TotalHotFiles: 0,
      Projects: 0,
    },
    Signals: {
      GradeShare: Array(n).fill({}),
      GPA: Array(n).fill(0),
      ArchetypeShare: Array(n).fill({}),
      CompletedRate: Array(n).fill(0),
      AbandonedRate: Array(n).fill(0),
      OutcomeTotal: Array(n).fill(0),
      CompletedCount: Array(n).fill(0),
      AbandonedCount: Array(n).fill(0),
      HygieneTerse: Array(n).fill(0),
      HygieneRepeated: Array(n).fill(0),
      HygieneNoCode: Array(n).fill(0),
      HygieneUnstructured: Array(n).fill(0),
      ContextResets: Array(n).fill(0),
      ContextHistogram: [],
      ContextMarkers: [],
    },
    Economics: {
      CostCompleted: Array(n).fill(0),
      CostAbandoned: Array(n).fill(0),
      CostOther: Array(n).fill(0),
      CacheSavings: Array(n).fill(0),
      CacheHitRate: Array(n).fill(0),
      CacheMeasured: Array(n).fill(true),
      TotalSpend: 0,
      TotalAbandoned: 0,
      AbandonedSharePct: 0,
      TotalCacheSavings: 0,
      CacheHitRateLatest: 0,
    },
    Subagents: {
      DelegateShare: Array(n).fill(0),
      CostShare: Array(n).fill(0),
      FanoutOrder: [],
      FanoutRows: Array(n).fill({}),
      SessionsThatDelegatePct: 0,
      SubagentSessionsInWindow: 0,
      CostThroughSubagentsPct: 0,
      DeepestTree: 0,
    },
    Rhythm: { Cells: Array.from({ length: 7 }, () => Array(24).fill(0)) },
  };
}

describe("FleetMixInstrument model identifiers", () => {
  it("keeps the short chip label but carries the full id in a title attribute", () => {
    render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            ["2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"],
            ["Jun 1", "Jun 8"],
          )}
        />
      </TooltipHost>,
    );

    const legend = document.querySelector(".legend") as HTMLElement;
    const chip = within(legend).getByText("sonnet-5").closest("li");
    expect(chip).toHaveAttribute("title", "claude-sonnet-5");

    const arrival = screen.getByText("haiku-4-5-20251001");
    expect(arrival.closest("span")).toHaveAttribute(
      "title",
      "claude-haiku-4-5-20251001",
    );
  });

  it("keeps the full id when stripping the vendor prefix would give two models one label", () => {
    render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            ["2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"],
            ["Jun 1", "Jun 8"],
            ["claude-sonnet-5", "sonnet-5"],
          )}
        />
      </TooltipHost>,
    );

    const legend = document.querySelector(".legend") as HTMLElement;
    expect(within(legend).getByText("claude-sonnet-5")).toBeInTheDocument();
    expect(within(legend).getByText("sonnet-5")).toBeInTheDocument();
  });

  it("titles the chart's own hover tooltip rows with the full id, same as the legend", () => {
    const { container } = render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            ["2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"],
            ["Jun 1", "Jun 8"],
          )}
        />
      </TooltipHost>,
    );

    hoverBucket(container, 0);

    const rows = document.querySelectorAll(".chart-tooltip .tt-row");
    const titles = [...rows].map((row) => row.getAttribute("title"));
    expect(titles).toContain("claude-sonnet-5");
    expect(titles).toContain("claude-opus-4-8");
  });
});

describe("FleetMixInstrument axis labels", () => {
  it("leaves labels alone when the window sits inside one calendar year", () => {
    render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            [
              "2026-01-01T00:00:00Z",
              "2026-06-01T00:00:00Z",
              "2026-12-01T00:00:00Z",
            ],
            ["Jan 1", "Jun 1", "Dec 1"],
          )}
        />
      </TooltipHost>,
    );

    const ticks = screen.getAllByText(/^(Jan 1|Jun 1|Dec 1)$/);
    expect(ticks).toHaveLength(3);
  });

  it("appends the year to every axis tick when the window crosses a year boundary", () => {
    render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            [
              "2025-07-01T00:00:00Z",
              "2026-01-01T00:00:00Z",
              "2026-06-01T00:00:00Z",
            ],
            ["Jul 1", "Jan 1", "Jun 1"],
          )}
        />
      </TooltipHost>,
    );

    expect(screen.getByText("Jul 1, 2025")).toBeInTheDocument();
    expect(screen.getByText("Jan 1, 2026")).toBeInTheDocument();
    expect(screen.getByText("Jun 1, 2026")).toBeInTheDocument();
  });

  it("titles the hover tooltip with the same year-suffixed label the axis shows", () => {
    const { container } = render(
      <TooltipHost>
        <FleetMixInstrument
          trends={trends(
            [
              "2025-07-01T00:00:00Z",
              "2026-01-01T00:00:00Z",
              "2026-06-01T00:00:00Z",
            ],
            ["Jul 1", "Jan 1", "Jun 1"],
          )}
        />
      </TooltipHost>,
    );

    hoverBucket(container, 0);

    expect(
      document.querySelector(".chart-tooltip .tt-title")?.textContent,
    ).toBe("Jul 1, 2025");
  });
});
