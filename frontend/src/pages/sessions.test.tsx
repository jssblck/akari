import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SessionRow } from "../types";
import { keysetCursorValue, SessionsPage } from "./sessions";

function row(overrides: Partial<SessionRow> = {}): SessionRow {
  return {
    ID: 1,
    Agent: "claude",
    Machine: "workstation",
    GitBranch: "main",
    Username: "grace",
    MessageCount: 12,
    UserMessageCount: 4,
    ModelFallbackCount: 0,
    TotalInput: 1000,
    TotalOutput: 500,
    TotalCacheWrite: 0,
    TotalCacheRead: 0,
    TotalCostUSD: 0.5,
    CostIncomplete: false,
    Visibility: "private",
    PublicID: null,
    StartedAt: null,
    EndedAt: null,
    LastActiveAt: new Date().toISOString(),
    Title: "Fix the flaky test",
    ProjectID: 1,
    ProjectKey: "github.com/org/akari",
    ProjectName: "akari",
    ProjectKind: "remote",
    Grade: null,
    Outcome: "completed",
    Search: { Text: "", MatchStart: 0, MatchEnd: 0 },
    Tree: { SubagentCount: 0, CostUSD: 0, CostIncomplete: false },
    ...overrides,
  };
}

function stubSessionsResponse(sessions: SessionRow[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        sessions,
        has_more: false,
        facets: { Agents: [], Machines: [], Users: [], Projects: [] },
      }),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("SessionsPage feed rendering", () => {
  it("serializes the observed value for every supported keyset order", () => {
    const session = row({
      MessageCount: 11,
      TotalInput: 10,
      TotalOutput: 20,
      TotalCacheWrite: 30,
      TotalCacheRead: 40,
      TotalCostUSD: 1.25,
      LastActiveAt: "2026-07-13T12:34:56.123456Z",
    });
    expect(keysetCursorValue("updated", session)).toBe(
      "2026-07-13T12:34:56.123456Z",
    );
    expect(keysetCursorValue("tokens", session)).toBe("100");
    expect(keysetCursorValue("messages", session)).toBe("11");
    expect(keysetCursorValue("cost", session)).toBe("1.25");
  });

  it("highlights the matched substring of a search snippet in a <mark>", async () => {
    stubSessionsResponse([
      row({
        ID: 1,
        Search: {
          Text: "Ada Lovelace wrote the first algorithm",
          MatchStart: 4,
          MatchEnd: 12,
        },
      }),
    ]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    const mark = await screen.findByText("Lovelace");
    expect(mark.tagName).toBe("MARK");
    expect(mark.closest(".srow-snippet")).toHaveTextContent(
      "Ada Lovelace wrote the first algorithm",
    );
  });

  it("falls back to the stripped title when there is no search snippet", async () => {
    stubSessionsResponse([
      row({ ID: 1, Search: { Text: "", MatchStart: 0, MatchEnd: 0 } }),
    ]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );
    expect(await screen.findByText("Fix the flaky test")).toBeInTheDocument();
  });

  it("groups rows under day headings when sorted by recency", async () => {
    const now = new Date();
    const tenDaysAgo = new Date(
      now.getFullYear(),
      now.getMonth(),
      now.getDate() - 10,
      12,
    );
    stubSessionsResponse([
      row({ ID: 1, LastActiveAt: now.toISOString(), Title: "Today's work" }),
      row({
        ID: 2,
        LastActiveAt: tenDaysAgo.toISOString(),
        Title: "Older work",
      }),
    ]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Today's work");
    const heads = document.querySelectorAll(".day-head");
    // Two sessions ten days apart fall into two distinct day buckets, so the
    // feed renders two headings rather than one flat list.
    expect(heads.length).toBe(2);
    expect(
      within(heads[0] as HTMLElement).getByText("Today"),
    ).toBeInTheDocument();
  });

  it("shows the session count footer", async () => {
    stubSessionsResponse([row({ ID: 1 }), row({ ID: 2 })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );
    expect(await screen.findByText("2 sessions")).toBeInTheDocument();
  });

  it("shows the empty state when the feed has no rows", async () => {
    stubSessionsResponse([]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );
    expect(await screen.findByText("No matching sessions")).toBeInTheDocument();
  });
});
