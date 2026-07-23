import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { Analytics, Breakdown } from "../types";
import { AnalyticsPanel } from "./overview";

function breakdown(
  label: string,
  overrides: Partial<Breakdown> = {},
): Breakdown {
  return {
    Label: label,
    CostUSD: 1,
    Input: 100,
    Output: 50,
    CacheRead: 10,
    CacheWrite: 5,
    Reasoning: 0,
    Sessions: 1,
    ...overrides,
  };
}

function analytics(overrides: Partial<Analytics> = {}): Analytics {
  return {
    Series: [
      {
        Day: "2026-07-01",
        CostUSD: 1.5,
        Input: 1000,
        Output: 500,
        CacheRead: 100,
        CacheWrite: 50,
      },
    ],
    Models: [breakdown("fable-5")],
    Agents: [breakdown("claude")],
    Users: null,
    TotalCost: 12.5,
    TotalIn: 5000,
    TotalOut: 2000,
    TotalCacheRead: 500,
    TotalCacheWrite: 200,
    TotalReasoning: 0,
    Sessions: 7,
    Cache: {
      Input: 5000,
      Output: 2000,
      CacheRead: 500,
      CacheWrite: 200,
      SavingsUSD: 3.25,
    },
    ...overrides,
  };
}

describe("AnalyticsPanel", () => {
  it("renders the stat strip, the activity heatmap, and the Models/Agents breakdowns", () => {
    render(<AnalyticsPanel analytics={analytics()} />);

    expect(screen.getByText("Cost")).toBeInTheDocument();
    // formatCost now holds a single fixed precision (two decimals) above a
    // cent, so $12.50 reads the same shape as every other cost in the column.
    expect(screen.getByText("$12.50")).toBeInTheDocument();
    expect(screen.getByText("Sessions")).toBeInTheDocument();
    expect(screen.getByText("7")).toBeInTheDocument();

    expect(screen.getByText("Daily activity")).toBeInTheDocument();
    expect(
      screen.getByRole("img", { name: "Daily activity" }),
    ).toBeInTheDocument();

    expect(screen.getByText("Models")).toBeInTheDocument();
    expect(screen.getByText("fable-5")).toBeInTheDocument();
    expect(screen.getByText("Agents")).toBeInTheDocument();
    expect(screen.getByText("claude")).toBeInTheDocument();
  });

  it("discloses the pre-cache cost behind the Cost stat, like Tokens and Cache hit already do", () => {
    render(<AnalyticsPanel analytics={analytics()} />);

    const costStat = screen.getByText("Cost").closest(".stat");
    expect(costStat?.querySelector(".hover-tip-summary")?.textContent).toBe(
      "$12.50",
    );
    // TotalCost (12.5) plus Cache.SavingsUSD (3.25): what the same usage
    // would have cost without caching.
    expect(costStat?.textContent).toContain("Without cache$15.75");
    expect(costStat?.textContent).toContain("saved around $3.25");
  });

  it("presents zero-priced unknown models as part of the estimate", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({
          Models: [breakdown("Other", { CostUSD: 0 })],
        })}
      />,
    );

    expect(screen.getByText("$12.50")).toBeInTheDocument();
    expect(screen.getByText("not priced")).toBeInTheDocument();
    expect(screen.getAllByText("saved around $3.25").length).toBeGreaterThan(0);
    expect(screen.queryByText(/partial|\$[\d.]+\+/i)).not.toBeInTheDocument();

    // The token figure's own hover card must not repeat the misleading exact
    // zero either: it should just omit the cost line, the same way the row
    // omits it in favour of "not priced".
    const row = screen.getByText("Other").closest(".breakdown-row");
    expect(row?.querySelector(".tt-cost")).not.toBeInTheDocument();
  });

  it("keeps the share fill in its own track instead of behind the row text", () => {
    const { container } = render(<AnalyticsPanel analytics={analytics()} />);

    const row = screen.getByText("fable-5").closest(".breakdown-row");
    const track = row?.querySelector(".breakdown-track");
    const fill = track?.querySelector(".breakdown-fill");
    expect(fill).toBeInTheDocument();
    // The head (label and cost) and sub (tokens and sessions) lines are not
    // nested inside the fill's track, so the fill can never paint under them.
    expect(track?.querySelector(".breakdown-head")).toBeNull();
    expect(track?.querySelector(".breakdown-sub")).toBeNull();
    expect(container.querySelector(".breakdown-fill")).toBe(fill);
  });

  it("places scoped controls in the activity header and marks its mobile presentation", () => {
    render(
      <AnalyticsPanel
        analytics={analytics()}
        activityControls={<span>Trailing window</span>}
        mobileActivity="range-only"
      />,
    );

    const activityPanel = screen.getByText("Daily activity").closest("section");
    expect(activityPanel).toHaveClass("range-only");
    expect(activityPanel).toContainElement(screen.getByText("Trailing window"));
  });

  it("hides the Users breakdown by default even with users present", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({
          Users: [breakdown("Grace Hopper"), breakdown("Ada Lovelace")],
        })}
      />,
    );
    expect(screen.queryByText("Users")).not.toBeInTheDocument();
  });

  it("hides the Users breakdown when showUsers is set but only one user is present", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({ Users: [breakdown("Grace Hopper")] })}
        showUsers
      />,
    );
    expect(screen.queryByText("Users")).not.toBeInTheDocument();
  });

  it("shows the Users breakdown when showUsers is set and 2+ users are present", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({
          Users: [breakdown("Grace Hopper"), breakdown("Ada Lovelace")],
        })}
        showUsers
      />,
    );
    expect(screen.getByText("Users")).toBeInTheDocument();
    expect(screen.getByText("Grace Hopper")).toBeInTheDocument();
    expect(screen.getByText("Ada Lovelace")).toBeInTheDocument();
  });

  it("renders the empty state when no usage has been recorded", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({
          Series: [],
          Sessions: 0,
          TotalCost: 0,
        })}
      />,
    );
    expect(screen.getByText("No usage recorded yet.")).toBeInTheDocument();
    expect(screen.queryByText("Daily activity")).not.toBeInTheDocument();
  });
});
