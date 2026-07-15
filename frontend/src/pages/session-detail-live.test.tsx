import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

const liveMocks = vi.hoisted(() => ({
  close: vi.fn(),
  watch: vi.fn(),
}));

vi.mock("../components/async-view", () => ({
  AsyncView: ({ state }: { state: { kind: string } }) => (
    <div data-kind={state.kind} data-testid="async-view" />
  ),
}));
vi.mock("./session-live", () => ({
  watchSessionUpdates: liveMocks.watch,
}));

import { SessionPage } from "./session-detail";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

afterEach(() => {
  vi.unstubAllGlobals();
  liveMocks.close.mockReset();
  liveMocks.watch.mockReset();
});

describe("SessionPage live startup", () => {
  it("waits for the matching initial snapshot before opening the stream", async () => {
    const initial = deferred<Response>();
    vi.stubGlobal(
      "fetch",
      vi.fn(() => initial.promise),
    );
    liveMocks.watch.mockReturnValue(liveMocks.close);

    render(
      <MemoryRouter initialEntries={["/sessions/7"]}>
        <Routes>
          <Route path="/sessions/:id" element={<SessionPage />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(liveMocks.watch).not.toHaveBeenCalled();

    initial.resolve(
      Response.json({
        snapshot: { Audit: { Detail: { ID: 7 } } },
        owner: true,
        can_delete: true,
      }),
    );
    await waitFor(() => expect(liveMocks.watch).toHaveBeenCalledOnce());
    expect(liveMocks.watch).toHaveBeenCalledWith(
      "7",
      expect.any(Function),
      expect.any(Function),
    );
  });

  it("does not attach a new route to a stale prior-session response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        Response.json({
          snapshot: { Audit: { Detail: { ID: 8 } } },
          owner: true,
          can_delete: true,
        }),
      ),
    );

    render(
      <MemoryRouter initialEntries={["/sessions/7"]}>
        <Routes>
          <Route path="/sessions/:id" element={<SessionPage />} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.getByTestId("async-view")).toHaveAttribute(
        "data-kind",
        "ready",
      ),
    );
    expect(liveMocks.watch).not.toHaveBeenCalled();
  });
});
