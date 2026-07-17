import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { AccountProject, AccountResponse } from "../types";
import { AccountPage } from "./account";

function accountResponse(projects: AccountProject[]): AccountResponse {
  return {
    connections: [],
    invites: [],
    projects,
    reparse: { done: 0, failed: 0, in_progress: false, total: 0 },
    tokens: [],
    user: {
      authenticated: true,
      csrf_token: "",
      is_admin: false,
      overview_public: false,
      user_id: 1,
      username: "grace",
      version: "dev",
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AccountPage project publication controls", () => {
  it("publishes a selected project and makes a public project private", async () => {
    let projects: AccountProject[] = [
      {
        id: 7,
        name: "akari",
        published: true,
        remote_key: "github.com/jssblck/akari",
      },
      {
        id: 8,
        name: "compiler",
        published: false,
        remote_key: "github.com/grace/compiler",
      },
    ];
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (init?.method === "PUT") {
          const id = Number(url.match(/projects\/(\d+)\/publication/)?.[1]);
          const { published } = JSON.parse(String(init.body)) as {
            published: boolean;
          };
          projects = projects.map((project) =>
            project.id === id ? { ...project, published } : project,
          );
          return Response.json({ published });
        }
        return Response.json(accountResponse(projects));
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<AccountPage />);

    const heading = await screen.findByRole("heading", {
      name: "Public project overviews",
    });
    const section = heading.closest("section");
    expect(section).not.toBeNull();
    const controls = within(section as HTMLElement);
    expect(controls.getByText("akari")).toBeInTheDocument();
    expect(controls.getByRole("link")).toHaveAttribute(
      "href",
      `${window.location.origin}/p/7`,
    );

    fireEvent.change(controls.getByRole("combobox"), {
      target: { value: "8" },
    });
    fireEvent.click(controls.getByRole("button", { name: "Publish" }));

    await controls.findByText("compiler");
    expect(
      controls.getAllByRole("button", { name: "Make private" }),
    ).toHaveLength(2);

    const akariRow = controls.getByText("akari").closest(".settings-row");
    expect(akariRow).not.toBeNull();
    fireEvent.click(
      within(akariRow as HTMLElement).getByRole("button", {
        name: "Make private",
      }),
    );

    await screen.findByRole("option", {
      name: "akari - github.com/jssblck/akari",
    });
    expect(
      controls.getAllByRole("button", { name: "Make private" }),
    ).toHaveLength(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/app/projects/8/publication",
      expect.objectContaining({
        body: JSON.stringify({ published: true }),
        method: "PUT",
      }),
    );
  });
});
