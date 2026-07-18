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
    CostIncomplete: false,
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
    CostIncomplete: false,
    Cache: {
      Input: 5000,
      Output: 2000,
      CacheRead: 500,
      CacheWrite: 200,
      SavingsUSD: 3.25,
      SavingsIncomplete: false,
    },
    ...overrides,
  };
}

describe("AnalyticsPanel", () => {
  it("renders the stat strip, the activity heatmap, and the Models/Agents breakdowns", () => {
    render(<AnalyticsPanel analytics={analytics()} />);

    expect(screen.getByText("Cost")).toBeInTheDocument();
    // TotalCost=12.5 sits in the $10-$100 band, which formatCost renders with
    // a single decimal digit.
    expect(screen.getByText("$12.5")).toBeInTheDocument();
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

  it("does not mark incomplete costs with a plus", () => {
    render(
      <AnalyticsPanel
        analytics={analytics({
          CostIncomplete: true,
          Models: [breakdown("unpriced", { CostIncomplete: true })],
        })}
      />,
    );

    expect(screen.getByText("$12.5")).toBeInTheDocument();
    expect(screen.getAllByText("$1.00").length).toBeGreaterThan(0);
    expect(screen.queryByText(/\$[\d.]+\+/)).not.toBeInTheDocument();
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
