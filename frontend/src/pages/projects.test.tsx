import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Project, ProjectsResponse } from "../types";
import { ProjectsPage } from "./projects";

function project(overrides: Partial<Project> = {}): Project {
  return {
    ID: 1,
    Owner: "jssblck",
    Repo: "akari",
    RemoteKey: "github.com/jssblck/akari",
    DisplayName: "akari",
    Kind: "remote",
    Host: "grace",
    SessionCount: 10,
    TotalInput: 1_000,
    TotalOutput: 500,
    TotalCacheRead: 0,
    TotalCacheWrite: 0,
    TotalCostUSD: 2.5,
    CostIncomplete: false,
    LastActivity: "2026-07-13T12:00:00Z",
    OverviewPublic: false,
    ...overrides,
  };
}

function stubProjectsResponse(response: ProjectsResponse) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json(response)),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ProjectsPage", () => {
  it("keeps repositories and local folders on the same measurement grid", async () => {
    stubProjectsResponse({
      projects: [
        project({ ID: 1 }),
        project({
          ID: 2,
          DisplayName: "art",
          Kind: "standalone",
          RemoteKey: "local:grace:C:\\Users\\me\\projects\\art",
        }),
      ],
      sparklines: { "1": [0, 2], "2": [1, 0] },
    });
    render(
      <MemoryRouter initialEntries={["/projects"]}>
        <ProjectsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Repositories");
    const tables = document.querySelectorAll(".projects-table");
    expect(tables).toHaveLength(2);
    expect(tables[0]?.querySelectorAll("th")).toHaveLength(6);
    expect(tables[1]?.querySelectorAll("th")).toHaveLength(6);
    expect(screen.queryByRole("columnheader", { name: "Cost" })).toBeNull();
    expect(
      screen.getAllByRole("columnheader", { name: "30 day tokens" }),
    ).toHaveLength(2);
    expect(document.querySelector(".project-kind-empty")).toBeInTheDocument();
    expect(
      document.querySelector(".project-kind .tag.standalone"),
    ).toHaveTextContent("standalone");
    const localPathSummary = document.querySelector(
      ".project-location-path.tail",
    );
    expect(localPathSummary).toHaveTextContent("C:\\Users\\me\\projects\\art");
    expect(
      localPathSummary
        ?.closest(".project-location-tip")
        ?.querySelector(".project-location-full"),
    ).toHaveTextContent("C:\\Users\\me\\projects\\art");
    expect(document.querySelector(".project-location-tip")).toHaveAttribute(
      "tabindex",
      "0",
    );
    expect(document.querySelector(".project-token-total")).toHaveTextContent(
      "1,500",
    );
  });

  it("searches names, paths, remotes, and hosts across both sections", async () => {
    stubProjectsResponse({
      projects: [
        project({ ID: 1, DisplayName: "akari" }),
        project({
          ID: 2,
          DisplayName: "scratchpad",
          Kind: "orphaned",
          Host: "ada",
          RemoteKey: "local:ada:C:\\Temp\\scratchpad",
        }),
      ],
      sparklines: {},
    });
    render(
      <MemoryRouter initialEntries={["/projects"]}>
        <ProjectsPage />
      </MemoryRouter>,
    );

    const search = await screen.findByLabelText("Search projects");
    fireEvent.change(search, { target: { value: "ada" } });
    expect(screen.getByText("scratchpad")).toBeInTheDocument();
    expect(
      Array.from(document.querySelectorAll(".primary-link")).map(
        (link) => link.textContent,
      ),
    ).toEqual(["scratchpad"]);
    expect(screen.getByText("1 project")).toBeInTheDocument();
  });

  it("sorts each section without mixing repositories and local folders", async () => {
    stubProjectsResponse({
      projects: [
        project({ ID: 1, DisplayName: "smaller", SessionCount: 2 }),
        project({ ID: 2, DisplayName: "larger", SessionCount: 20 }),
      ],
      sparklines: {},
    });
    render(
      <MemoryRouter initialEntries={["/projects"]}>
        <ProjectsPage />
      </MemoryRouter>,
    );

    const sort = await screen.findByLabelText("Sort projects");
    fireEvent.change(sort, { target: { value: "sessions" } });
    const section = screen.getByText("Repositories").closest("section");
    expect(section).not.toBeNull();
    const links = within(section as HTMLElement).getAllByRole("link");
    expect(links.map((link) => link.textContent)).toEqual([
      "larger",
      "smaller",
    ]);
  });

  it("bounds daily token bars and explains their scale", async () => {
    stubProjectsResponse({
      projects: [project({ ID: 1 })],
      sparklines: { "1": [0, 1, 100] },
    });
    render(
      <MemoryRouter initialEntries={["/projects"]}>
        <ProjectsPage />
      </MemoryRouter>,
    );

    await screen.findByText("Repositories");
    const bars = document.querySelectorAll(".activity-bars rect");
    expect(bars).toHaveLength(3);
    for (const bar of bars) {
      const y = Number(bar.getAttribute("y"));
      const height = Number(bar.getAttribute("height"));
      expect(y).toBeGreaterThanOrEqual(0);
      expect(y + height).toBeLessThanOrEqual(24);
    }
    expect(screen.getByText("Active days").nextSibling).toHaveTextContent("2");
    expect(screen.getByText("Total tokens").nextSibling).toHaveTextContent(
      "101",
    );
    expect(screen.getByText("Peak day").nextSibling).toHaveTextContent("100");
  });
});
