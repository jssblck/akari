import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import type { DateRange } from "../types";
import { RangeTabs } from "./range-tabs";

const ranges: DateRange[] = [
  { Key: "30d", Label: "30 days", Days: 30 },
  { Key: "year", Label: "Year", Days: 365 },
];

function renderAt(url: string, active: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <RangeTabs ranges={ranges} active={active} />
    </MemoryRouter>,
  );
}

describe("RangeTabs", () => {
  it("marks the server-declared range active when the URL has no param", () => {
    renderAt("/overview", "year");
    expect(screen.getByRole("button", { name: "Year" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "30 days" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("lets the URL param win over the server echo so a click flips before the refetch lands", () => {
    renderAt("/overview?range=30d", "year");
    expect(screen.getByRole("button", { name: "30 days" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Year" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });
});
