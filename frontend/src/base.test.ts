import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  delete window.__AKARI_BASE_PATH__;
  vi.resetModules();
});

describe("absoluteURL", () => {
  it("builds a root-mounted URL", async () => {
    const { absoluteURL } = await import("./base");
    expect(absoluteURL("/s/ada")).toBe(`${window.location.origin}/s/ada`);
  });

  it("keeps the injected deployment prefix", async () => {
    window.__AKARI_BASE_PATH__ = "/proxy/akari";
    const { absoluteURL } = await import("./base");
    expect(absoluteURL("/s/ada")).toBe(
      `${window.location.origin}/proxy/akari/s/ada`,
    );
  });

  it("keeps an encoded username inside one path segment", async () => {
    window.__AKARI_BASE_PATH__ = "/proxy/akari";
    const { absoluteURL } = await import("./base");
    const username = "Ada /?#";
    expect(absoluteURL(`/u/${encodeURIComponent(username)}`)).toBe(
      `${window.location.origin}/proxy/akari/u/Ada%20%2F%3F%23`,
    );
  });
});
