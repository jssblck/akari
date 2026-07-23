import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Insights } from "../types";
import { InsightsPage } from "./insights";

function insights(): Insights {
  return {
    Quality: { Grades: [], Outcomes: [], Sessions: 0, Graded: 0 },
    Archetypes: [],
    Concurrency: {
      FleetPeak: 0,
      FleetPeakAt: "",
      BusiestUser: "",
      BusiestUserPeak: 0,
      AvgConcurrent: 0,
      Sessions: 0,
    },
    Velocity: {
      ResponseP50: 0,
      ResponseP90: 0,
      FirstResponseP50: 0,
      MsgsPerActiveMin: 0,
      ToolsPerActiveMin: 0,
      ActiveSeconds: 0,
      Turns: 0,
      Sessions: 0,
    },
    Tools: { TotalCalls: 0, TotalFailures: 0, Turns: 0, Tools: [], Clipped: 0 },
    Hygiene: {
      Prompts: 0,
      Short: 0,
      Duplicate: 0,
      NoCodeContext: 0,
      Sessions: 0,
      UnstructuredStarts: 0,
    },
    Churn: { Files: [], Clipped: 0 },
    Context: {
      Sessions: 0,
      PeakTokensP50: 0,
      PeakTokensP90: 0,
      PeakTokensMax: 0,
      TotalResets: 0,
      SessionsWithReset: 0,
    },
    Trends: null,
  };
}

function stubInsightsResponse(range: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        insights: insights(),
        generated_at: "2026-07-01T00:00:00Z",
        range,
        ranges: [
          { Key: "7d", Label: "7 days", Days: 7 },
          { Key: "30d", Label: "30 days", Days: 30 },
          { Key: "90d", Label: "90 days", Days: 90 },
          { Key: "year", Label: "Year", Days: 365 },
          { Key: "all", Label: "All", Days: 0 },
        ],
      }),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("InsightsPage default range", () => {
  it("requests 90 days when the URL carries no range, without touching the URL bar", async () => {
    stubInsightsResponse("90d");
    render(
      <MemoryRouter initialEntries={["/insights"]}>
        <InsightsPage />
      </MemoryRouter>,
    );

    await screen.findByText("No sessions in this window yet.");
    const fetchMock = vi.mocked(fetch);
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("range=90d");
    expect(screen.getByRole("button", { name: "90 days" })).toHaveClass(
      "active",
    );
  });

  it("leaves an explicit range in the URL alone", async () => {
    stubInsightsResponse("year");
    render(
      <MemoryRouter initialEntries={["/insights?range=year"]}>
        <InsightsPage />
      </MemoryRouter>,
    );

    await screen.findByText("No sessions in this window yet.");
    const fetchMock = vi.mocked(fetch);
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("range=year");
  });
});
