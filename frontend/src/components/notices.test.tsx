import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RequestError } from "../api";
import { attempt, dismissNotice, NoticeHost, notify } from "./notices";

// notify()/attempt() keep their queue at module scope so any code can raise a
// notice without a hook, which means the queue also outlives any single
// test's render. Sweep every id a test in this file could plausibly have
// minted so the next test always starts from an empty stack.
afterEach(() => {
  for (let id = 1; id <= 200; id++) dismissNotice(id);
});

describe("notify", () => {
  it("keeps an err notice on screen until it is dismissed", async () => {
    const user = userEvent.setup();
    render(<NoticeHost />);
    act(() => {
      notify("Could not save.", "err");
    });

    expect(screen.getByText("Could not save.")).toBeInTheDocument();
    // An error notice carries no auto-dismiss timer; it should still be
    // present after a beat.
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(screen.getByText("Could not save.")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("Could not save.")).not.toBeInTheDocument();
  });

  it("auto-dismisses an ok notice after its timeout", () => {
    vi.useFakeTimers();
    try {
      render(<NoticeHost />);
      act(() => {
        notify("Saved.", "ok");
      });
      expect(screen.getByText("Saved.")).toBeInTheDocument();

      act(() => {
        vi.advanceTimersByTime(5000);
      });
      expect(screen.queryByText("Saved.")).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("attempt", () => {
  it("resolves true and raises no err notice on success", async () => {
    render(<NoticeHost />);
    const ok = await attempt(Promise.resolve("done"), "Saved.");
    expect(ok).toBe(true);
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
  });

  it("resolves false and raises an err notice for a RequestError", async () => {
    render(<NoticeHost />);
    const ok = await attempt(
      Promise.reject(new RequestError(400, "Bad title.")),
    );
    expect(ok).toBe(false);
    expect(await screen.findByText("Bad title.")).toBeInTheDocument();
  });

  it("falls back to a generic message for a non-RequestError failure", async () => {
    render(<NoticeHost />);
    const ok = await attempt(Promise.reject(new Error("network down")));
    expect(ok).toBe(false);
    expect(
      await screen.findByText(
        "Request failed; check your connection and try again.",
      ),
    ).toBeInTheDocument();
  });
});
