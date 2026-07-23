import { describe, expect, it } from "vitest";

import { formatCost, formatCount, formatPercent, formatTokens } from "./format";

describe("formatCost", () => {
  it("keeps four decimals for a sub-cent cost so it doesn't round to $0.00", () => {
    expect(formatCost(0.0042)).toBe("$0.0042");
  });

  it("does not take the sub-cent path at exactly zero", () => {
    expect(formatCost(0)).toBe("$0.00");
  });

  it("uses two decimal digits under $10", () => {
    expect(formatCost(1.5)).toBe("$1.50");
  });

  it("keeps two decimal digits from $10 up to $100", () => {
    expect(formatCost(12.34)).toBe("$12.34");
  });

  it("keeps two decimal digits at $100 and above", () => {
    expect(formatCost(123.4)).toBe("$123.40");
  });

  it("keeps two decimal digits into the thousands", () => {
    expect(formatCost(5925)).toBe("$5,925.00");
  });
});

describe("formatTokens", () => {
  it("formats billions with a B suffix", () => {
    expect(formatTokens(2_500_000_000)).toBe("2.5B");
  });

  it("formats millions with an M suffix", () => {
    expect(formatTokens(3_400_000)).toBe("3.4M");
  });

  it("formats thousands with a k suffix", () => {
    expect(formatTokens(12_300)).toBe("12.3k");
  });

  it("leaves sub-thousand counts as a plain integer", () => {
    expect(formatTokens(742)).toBe("742");
  });

  it("sits right at the k threshold", () => {
    expect(formatTokens(1000)).toBe("1.0k");
  });

  it("sits right at the M threshold", () => {
    expect(formatTokens(1_000_000)).toBe("1.0M");
  });

  it("sits right at the B threshold", () => {
    expect(formatTokens(1_000_000_000)).toBe("1.0B");
  });
});

describe("formatCount", () => {
  it("spells out counts under ten thousand in full", () => {
    expect(formatCount(9_999)).toBe("9,999");
  });

  it("switches to compact notation at ten thousand", () => {
    expect(formatCount(12_500)).toBe("12.5K");
  });

  it("formats a small count with no grouping needed", () => {
    expect(formatCount(42)).toBe("42");
  });
});

describe("formatPercent", () => {
  it("formats a fraction as a whole-number-leaning percent", () => {
    expect(formatPercent(0.5)).toBe("50%");
  });

  it("keeps at most one decimal digit", () => {
    expect(formatPercent(0.1234)).toBe("12.3%");
  });

  it("formats zero", () => {
    expect(formatPercent(0)).toBe("0%");
  });
});
