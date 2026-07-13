// Runs before every test file: wires jest-dom's matchers (toBeInTheDocument,
// toHaveClass, ...) into vitest's expect, and returns the DOM to empty
// between tests so one test's render can't leak into the next.
import { cleanup } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";

afterEach(() => {
  cleanup();
});
