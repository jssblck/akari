import { act, fireEvent, render, screen, within } from "@testing-library/react";
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
  it("debounces transcript searches while Enter still submits immediately", async () => {
    stubSessionsResponse([row({ ID: 1 })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Fix the flaky test");
    const fetchMock = vi.mocked(fetch);
    const search = screen.getByLabelText("Search session content");
    vi.useFakeTimers();
    try {
      fireEvent.change(search, { target: { value: "ada" } });
      await act(async () => vi.advanceTimersByTimeAsync(249));
      expect(fetchMock).toHaveBeenCalledOnce();
      await act(async () => vi.advanceTimersByTimeAsync(1));
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(String(fetchMock.mock.calls[1]?.[0])).toContain("q=ada");

      fireEvent.change(search, { target: { value: "grace" } });
      await act(async () =>
        fireEvent.submit(search.closest("form") as HTMLFormElement),
      );
      expect(fetchMock).toHaveBeenCalledTimes(3);
      expect(String(fetchMock.mock.calls[2]?.[0])).toContain("q=grace");
    } finally {
      vi.useRealTimers();
    }
  });

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

  it("uses fixed signal columns without the message and token summary", async () => {
    stubSessionsResponse([row({ ID: 1 })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Fix the flaky test");
    const signals = document.querySelector(".srow-signals");
    expect(signals).toHaveTextContent("Completed");
    expect(signals?.children).toHaveLength(4);
    expect(signals).not.toHaveTextContent("12 messages");
    expect(signals).not.toHaveTextContent("1.5k");
  });

  it("keeps fan-out cost detail in the popover instead of the column", async () => {
    stubSessionsResponse([
      row({
        ID: 1,
        Tree: { SubagentCount: 100, CostUSD: 42, CostIncomplete: false },
      }),
    ]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    const fanout = await screen.findByText("100 subagents");
    expect(fanout).not.toHaveTextContent("$");
    expect(screen.getByText("Total cost").nextSibling).toHaveTextContent("$42");
  });

  it("reserves a signal column for non-remote project kinds", async () => {
    stubSessionsResponse([row({ ID: 1, ProjectKind: "orphaned" })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Fix the flaky test");
    expect(
      document.querySelector(".srow-kind .tag.orphaned"),
    ).toHaveTextContent("orphaned");
    expect(
      screen.getByText("The working directory no longer exists on disk."),
    ).toBeInTheDocument();
    expect(document.querySelector(".srow-line .orphaned")).toBeNull();
  });

  it("includes useful overview stats in the row popover", async () => {
    stubSessionsResponse([row({ ID: 1 })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Fix the flaky test");
    const overview = document.querySelector(".session-overview");
    expect(overview).toHaveTextContent("Session overview");
    expect(overview).toHaveTextContent("Messages12");
    expect(overview).toHaveTextContent("User prompts4");
    expect(overview).toHaveTextContent("Total tokens1.5k");
    expect(overview).toHaveTextContent("Session cost$0.50");
    expect(overview).toHaveTextContent("claude on workstation");
  });

  it("uses a styled local date popover without native title tooltips", async () => {
    const lastActive = "2026-07-13T12:34:56Z";
    stubSessionsResponse([row({ ID: 1, LastActiveAt: lastActive })]);
    render(
      <MemoryRouter initialEntries={["/sessions"]}>
        <SessionsPage />
      </MemoryRouter>,
    );

    const title = await screen.findByText("Fix the flaky test");
    const expectedDate = new Intl.DateTimeFormat(undefined, {
      dateStyle: "full",
      timeStyle: "short",
    }).format(new Date(lastActive));
    expect(title).not.toHaveAttribute("title");
    expect(document.querySelector(".srow-date")).not.toHaveAttribute("title");
    expect(screen.getByText("Last active")).toBeInTheDocument();
    expect(screen.getByText(expectedDate)).toBeInTheDocument();
  });

  it("opens and closes the overview when the row content is hovered", async () => {
    const showPopover = vi.fn();
    const hidePopover = vi.fn();
    const originalShow = Object.getOwnPropertyDescriptor(
      HTMLElement.prototype,
      "showPopover",
    );
    const originalHide = Object.getOwnPropertyDescriptor(
      HTMLElement.prototype,
      "hidePopover",
    );
    Object.defineProperty(HTMLElement.prototype, "showPopover", {
      configurable: true,
      value: showPopover,
    });
    Object.defineProperty(HTMLElement.prototype, "hidePopover", {
      configurable: true,
      value: hidePopover,
    });

    try {
      stubSessionsResponse([row({ ID: 1 })]);
      render(
        <MemoryRouter initialEntries={["/sessions"]}>
          <SessionsPage />
        </MemoryRouter>,
      );

      await screen.findByText("Fix the flaky test");
      const rowElement = document.querySelector(".srow");
      const overview = document.querySelector(
        ".session-overview",
      ) as HTMLElement | null;
      expect(rowElement).not.toBeNull();
      expect(overview).not.toBeNull();
      let anchorTop = 300;
      vi.spyOn(
        rowElement as Element,
        "getBoundingClientRect",
      ).mockImplementation(
        () =>
          ({
            top: anchorTop,
            bottom: anchorTop + 65,
            left: 50,
            right: 850,
            width: 800,
            height: 65,
            x: 50,
            y: anchorTop,
            toJSON: () => ({}),
          }) as DOMRect,
      );
      vi.spyOn(
        overview as HTMLElement,
        "getBoundingClientRect",
      ).mockReturnValue({
        top: 0,
        bottom: 100,
        left: 0,
        right: 300,
        width: 300,
        height: 100,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      } as DOMRect);

      fireEvent.mouseOver(rowElement as Element, {
        clientX: 400,
        clientY: 300,
      });
      expect(showPopover).toHaveBeenCalledOnce();
      expect(overview?.style.top).toBe("192px");
      expect(overview?.style.left).toBe("250px");

      fireEvent.mouseOver(rowElement as Element, {
        clientX: 700,
        clientY: 500,
      });
      expect(overview?.style.top).toBe("192px");
      expect(overview?.style.left).toBe("250px");
      fireEvent.mouseLeave(rowElement as Element);
      expect(hidePopover).toHaveBeenCalledOnce();

      anchorTop = 20;
      fireEvent.mouseOver(rowElement as Element, {
        clientX: 400,
        clientY: 20,
      });
      expect(overview?.style.top).toBe("93px");

      const date = document.querySelector(".srow-date");
      expect(date).not.toBeNull();
      fireEvent.mouseOver(date as Element);
      expect(hidePopover).toHaveBeenCalledTimes(2);
    } finally {
      vi.restoreAllMocks();
      if (originalShow)
        Object.defineProperty(
          HTMLElement.prototype,
          "showPopover",
          originalShow,
        );
      else Reflect.deleteProperty(HTMLElement.prototype, "showPopover");
      if (originalHide)
        Object.defineProperty(
          HTMLElement.prototype,
          "hidePopover",
          originalHide,
        );
      else Reflect.deleteProperty(HTMLElement.prototype, "hidePopover");
    }
  });
});
