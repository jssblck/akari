import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useAPI } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// A minimal component so useAPI's returned LoadState can be asserted through
// rendered text rather than reaching into hook internals.
function Probe({ path }: { path: string }) {
  const state = useAPI<{ value: string }>(path);
  switch (state.kind) {
    case "loading":
      return <div>loading</div>;
    case "error":
      return <div>error: {state.error.message}</div>;
    case "gated":
      return (
        <div>
          gated: {state.reparse.done}/{state.reparse.total}
        </div>
      );
    case "ready":
      return <div>ready: {state.data.value}</div>;
  }
}

describe("useAPI", () => {
  it("transitions from loading to ready once the fetch resolves", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ value: "grace" })),
    );
    render(<Probe path="/api/v1/thing" />);
    expect(screen.getByText("loading")).toBeInTheDocument();
    expect(await screen.findByText("ready: grace")).toBeInTheDocument();
  });

  it("moves to the error state for a non-gated failure", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ error: "not found" }, { status: 404 })),
    );
    render(<Probe path="/api/v1/missing" />);
    expect(await screen.findByText("error: not found")).toBeInTheDocument();
  });

  it("enters the gated state on a 503 reparse body and schedules a refetch", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        call += 1;
        if (call === 1) {
          return Response.json(
            {
              error: "rebuilding",
              reparse: { in_progress: true, done: 3, total: 10, failed: 0 },
            },
            { status: 503 },
          );
        }
        return Response.json({ value: "grace" });
      }),
    );

    render(<Probe path="/api/v1/gated" />);
    expect(await screen.findByText("gated: 3/10")).toBeInTheDocument();
    expect(call).toBe(1);

    // useAPI polls the gated state on a fixed 5s interval until the rebuild
    // finishes, at which point the same path resolves normally.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(await screen.findByText("ready: grace")).toBeInTheDocument();
    expect(call).toBe(2);
  });
});
