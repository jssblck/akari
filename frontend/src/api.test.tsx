import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { parseRetryAfter, requestWithRetry, useAPI, waitForRetry } from "./api";

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

describe("requestWithRetry", () => {
  it("retries a structured rebuild response and honors Retry-After", async () => {
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        call += 1;
        if (call === 1) {
          return Response.json(
            {
              error: "rebuilding",
              reparse: { in_progress: true, done: 2, total: 9, failed: 0 },
            },
            { status: 503, headers: { "Retry-After": "2" } },
          );
        }
        return Response.json({ value: "ready" });
      }),
    );
    const delays: number[] = [];

    const result = await requestWithRetry<{ value: string }>(
      "/api/v1/transcript",
      {},
      async (delay) => {
        delays.push(delay);
      },
    );

    expect(result.value).toBe("ready");
    expect(delays).toEqual([2000]);
  });

  it("surfaces an unrelated 503 without retrying", async () => {
    const fetchMock = vi.fn(async () =>
      Response.json({ error: "overloaded" }, { status: 503 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(requestWithRetry("/api/v1/transcript")).rejects.toThrow(
      "overloaded",
    );
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("retry timing", () => {
  it("parses seconds and HTTP dates", () => {
    expect(parseRetryAfter("1.5")).toBe(1500);
    expect(parseRetryAfter(null)).toBe(0);
    expect(parseRetryAfter("invalid")).toBe(0);
  });

  it("removes its abort listener after the timer resolves", async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const remove = vi.spyOn(controller.signal, "removeEventListener");
    const pending = waitForRetry(25, controller.signal);

    await vi.advanceTimersByTimeAsync(25);
    await pending;

    expect(remove).toHaveBeenCalledWith("abort", expect.any(Function));
  });
});
