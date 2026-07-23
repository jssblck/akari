import { describe, expect, it } from "vitest";

import { formatSavings } from "./pricing-format";

describe("formatSavings", () => {
  it("presents positive savings as an estimate", () => {
    expect(formatSavings(92642)).toBe("saved around $92,642.00");
  });

  it("presents a negative saving as an estimated cost", () => {
    expect(formatSavings(-3.25)).toBe("cost around $3.25");
  });
});
