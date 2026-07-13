import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { DayPoint } from "../types";
import { ActivityHeatmap } from "./activity-heatmap";

const DAY_MS = 86400000;

// The current UTC calendar day, computed independently of the component (from
// UTC date parts, matching the server's UTC-truncated buckets) so these tests
// would catch the component drifting back to local date parts, which lag a
// UTC day behind for negative-offset hosts.
function todayUTC(): number {
  const now = new Date();
  return Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
}

function dayKey(offsetFromToday: number): string {
  const d = new Date(todayUTC() - offsetFromToday * DAY_MS);
  const m = d.getUTCMonth() + 1;
  const day = d.getUTCDate();
  return `${d.getUTCFullYear()}-${m < 10 ? `0${m}` : m}-${day < 10 ? `0${day}` : day}`;
}

function point(offsetFromToday: number, fields: Partial<DayPoint>): DayPoint {
  return {
    Day: dayKey(offsetFromToday),
    CostUSD: 0,
    Input: 0,
    Output: 0,
    CacheRead: 0,
    CacheWrite: 0,
    ...fields,
  };
}

// A full trailing year with a low, uniform baseline (so every visible cell
// gets at least lvl-1) and one deliberate spike far above everything else,
// so the spike day is the only cell that can reach lvl-4.
function yearFixture(spikeOffset: number, spikeTokens: number): DayPoint[] {
  const days: DayPoint[] = [];
  for (let i = 0; i < 400; i++) {
    days.push(
      i === spikeOffset
        ? point(i, { Input: spikeTokens })
        : point(i, { Input: 2 }),
    );
  }
  return days;
}

describe("ActivityHeatmap", () => {
  it("renders 365+ day cells for a year of fixture data", () => {
    const { container } = render(
      <ActivityHeatmap series={yearFixture(10, 1000)} metric="tokens" />,
    );
    const cells = container.querySelectorAll("rect.hm-cell");
    // The grid spans 53 weeks ending on the current week; only days beyond
    // today are left unrendered, so the count always lands in [365, 371].
    expect(cells.length).toBeGreaterThanOrEqual(365);
    expect(cells.length).toBeLessThanOrEqual(371);
  });

  it("renders the empty-state message when the series has no dated usage", () => {
    render(<ActivityHeatmap series={[]} metric="tokens" />);
    expect(screen.getByText("No dated usage to chart.")).toBeInTheDocument();
  });

  it("renders the empty-state message for a null series", () => {
    render(<ActivityHeatmap series={null} metric="tokens" />);
    expect(screen.getByText("No dated usage to chart.")).toBeInTheDocument();
  });

  it("assigns lvl-4 to the known peak day on the sqrt ramp", () => {
    const { container } = render(
      <ActivityHeatmap series={yearFixture(10, 1000)} metric="tokens" />,
    );
    // The spike (tokens=1000) sits at f=1 against the baseline (tokens=2,
    // f=sqrt(0.002)~0.045), so exactly one cell should clear the top band.
    const peak = container.querySelectorAll("rect.hm-cell.lvl-4");
    expect(peak.length).toBe(1);
    const baseline = container.querySelectorAll("rect.hm-cell.lvl-1");
    expect(baseline.length).toBeGreaterThan(300);
  });

  it("switches which day peaks when the metric prop flips from tokens to cost", () => {
    // Day A: cheap in cost, heavy in tokens. Day B: the reverse.
    const series = [
      point(0, { Input: 1000, CostUSD: 0.01 }),
      point(1, { Input: 0, CostUSD: 100 }),
    ];
    const byTokens = render(
      <ActivityHeatmap series={series} metric="tokens" />,
    );
    expect(
      byTokens.container.querySelectorAll("rect.hm-cell.lvl-4").length,
    ).toBe(1);
    byTokens.unmount();

    const byCost = render(<ActivityHeatmap series={series} metric="cost" />);
    // Day A is now the cheap day: it drops off the top band while day B (the
    // costly one) takes over lvl-4, so the peak moves when metric flips.
    expect(byCost.container.querySelectorAll("rect.hm-cell.lvl-4").length).toBe(
      1,
    );
    expect(byCost.container.querySelectorAll("rect.hm-cell.lvl-1").length).toBe(
      1,
    );
  });

  it("keys the grid to the UTC calendar day, not the host's local day", () => {
    // Pin the clock just past UTC midnight. On any west-of-UTC host the local
    // calendar day is still yesterday, so a component that built "today" from
    // local date parts would drop this point; keying by UTC keeps it.
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-02T00:30:00Z"));
    try {
      const { container } = render(
        <ActivityHeatmap
          series={[
            {
              Day: "2026-07-02",
              CostUSD: 0,
              Input: 500,
              Output: 0,
              CacheRead: 0,
              CacheWrite: 0,
            },
          ]}
          metric="tokens"
        />,
      );
      expect(
        container.querySelectorAll("rect.hm-cell.lvl-4").length,
      ).toBeGreaterThanOrEqual(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("renders month labels along the axis", () => {
    const { container } = render(
      <ActivityHeatmap series={yearFixture(10, 1000)} metric="tokens" />,
    );
    const labels = container.querySelectorAll("text.hm-month");
    // 53 weeks span just over a year, so at least 10 distinct month labels
    // should appear (they are spaced at least 3 columns apart).
    expect(labels.length).toBeGreaterThanOrEqual(10);
  });
});
