import { describe, expect, it } from "vitest";

import { safeNext } from "./auth";

describe("safeNext", () => {
  it("keeps same-origin application paths", () => {
    expect(safeNext("/sessions/42?range=30d#message-3")).toBe(
      "/sessions/42?range=30d#message-3",
    );
  });

  it.each([
    "//evil.example",
    "/\\evil.example",
    "/%5cevil.example",
  ])("rejects ambiguous external target %s", (target) => {
    expect(safeNext(target)).toBe("/overview");
  });
});
