import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { AccountProject, AccountResponse, Token } from "../types";
import { AccountPage } from "./account";

function accountResponse(
  projects: AccountProject[],
  tokens: Token[] = [],
): AccountResponse {
  return {
    connections: [],
    invites: [],
    projects,
    reparse: { done: 0, failed: 0, in_progress: false, total: 0 },
    tokens,
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

  it("gives the disabled Publish button the same weight as Make private", async () => {
    const fetchMock = vi.fn(async () =>
      Response.json(
        accountResponse([
          {
            id: 7,
            name: "akari",
            published: false,
            remote_key: "github.com/jssblck/akari",
          },
        ]),
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<AccountPage />);

    const heading = await screen.findByRole("heading", {
      name: "Public project overviews",
    });
    const section = heading.closest("section");
    expect(section).not.toBeNull();
    const publish = within(section as HTMLElement).getByRole("button", {
      name: "Publish",
    });
    expect(publish).toHaveClass("button", "secondary");
  });
});

describe("AccountPage token controls", () => {
  function tokens(): Token[] {
    return [
      {
        CreatedAt: "2026-01-01T00:00:00Z",
        ID: 1,
        LastUsedAt: null,
        Name: "ci-ingest",
        RevokedAt: null,
        Scope: "ingest",
      },
      {
        CreatedAt: "2025-06-01T00:00:00Z",
        ID: 2,
        LastUsedAt: null,
        Name: "old-full",
        RevokedAt: "2025-12-01T00:00:00Z",
        Scope: "full",
      },
      {
        CreatedAt: "2025-06-02T00:00:00Z",
        ID: 3,
        LastUsedAt: null,
        Name: "old-read",
        RevokedAt: "2025-12-02T00:00:00Z",
        Scope: "read",
      },
    ];
  }

  it("disables Create until a token name is entered", async () => {
    const fetchMock = vi.fn(async () => Response.json(accountResponse([])));
    vi.stubGlobal("fetch", fetchMock);

    render(<AccountPage />);

    const heading = await screen.findByRole("heading", { name: "API tokens" });
    const section = heading.closest("section");
    expect(section).not.toBeNull();
    const controls = within(section as HTMLElement);

    const create = controls.getByRole("button", { name: "Create" });
    expect(create).toBeDisabled();

    fireEvent.change(controls.getByPlaceholderText("Token name"), {
      target: { value: "laptop" },
    });
    expect(create).toBeEnabled();

    fireEvent.change(controls.getByPlaceholderText("Token name"), {
      target: { value: "   " },
    });
    expect(create).toBeDisabled();
  });

  it("keeps revoked tokens out of the active list, folded behind a count", async () => {
    const fetchMock = vi.fn(async () =>
      Response.json(accountResponse([], tokens())),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<AccountPage />);

    const heading = await screen.findByRole("heading", { name: "API tokens" });
    const section = heading.closest("section");
    expect(section).not.toBeNull();
    const controls = within(section as HTMLElement);

    expect(await controls.findByText("ci-ingest")).toBeVisible();

    const summary = controls.getByText("2 revoked");
    expect(controls.getByText("old-full")).not.toBeVisible();
    expect(controls.getByText("old-read")).not.toBeVisible();

    fireEvent.click(summary);
    expect(controls.getByText("old-full")).toBeVisible();
    expect(controls.getByText("old-read")).toBeVisible();
  });
});

describe("AccountPage public overview sharing copy", () => {
  it("describes what publishing exposes instead of repeating the Private status", async () => {
    const fetchMock = vi.fn(async () => Response.json(accountResponse([])));
    vi.stubGlobal("fetch", fetchMock);

    render(<AccountPage />);

    const heading = await screen.findByRole("heading", {
      name: "Public overview",
    });
    const section = heading.closest("section");
    expect(section).not.toBeNull();
    const controls = within(section as HTMLElement);

    expect(controls.getByText("Private")).toBeInTheDocument();
    expect(controls.queryByText("No public overview.")).not.toBeInTheDocument();
    expect(controls.getByText(/publishing shows/i)).toBeInTheDocument();
  });
});
