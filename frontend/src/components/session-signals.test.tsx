import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { SessionOutcome } from "./session-signals";

describe("SessionOutcome", () => {
  it("labels a session with no outcome yet and no end time as running, not unknown", () => {
    render(<SessionOutcome outcome="" endedAt={null} />);
    expect(screen.getAllByText("Running").length).toBeGreaterThan(0);
    expect(
      document.querySelector(".srow-outcome.outcome-running"),
    ).toBeInTheDocument();
  });

  it("keeps the unknown treatment for an ended session with no recognized outcome", () => {
    const { container } = render(
      <SessionOutcome outcome="" endedAt="2026-07-21T00:00:00Z" />,
    );
    expect(container.querySelector(".outcome-running")).not.toBeInTheDocument();
    expect(container.querySelector(".outcome-")).toBeInTheDocument();
  });

  it("still renders completed, abandoned, and errored as before", () => {
    const { rerender } = render(
      <SessionOutcome outcome="completed" endedAt="2026-07-21T00:00:00Z" />,
    );
    expect(screen.getAllByText("Completed").length).toBeGreaterThan(0);

    rerender(
      <SessionOutcome outcome="abandoned" endedAt="2026-07-21T00:00:00Z" />,
    );
    expect(screen.getAllByText("Abandoned").length).toBeGreaterThan(0);

    rerender(
      <SessionOutcome outcome="errored" endedAt="2026-07-21T00:00:00Z" />,
    );
    expect(screen.getAllByText("Errored").length).toBeGreaterThan(0);
  });
});
