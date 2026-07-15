import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { SessionResponse } from "../types";

const mocks = vi.hoisted(() => ({
  notify: vi.fn(),
  request: vi.fn(),
}));

vi.mock("../api", () => ({ request: mocks.request }));
vi.mock("../components/notices", () => ({ notify: mocks.notify }));

import { watchSessionUpdates } from "./session-live";

type EventListener = (event: Event) => void;

class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly url: string;
  readonly close = vi.fn();
  private listeners = new Map<string, Set<EventListener>>();

  constructor(url: string | URL) {
    this.url = String(url);
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: EventListener) {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener) {
    this.listeners.get(type)?.delete(listener);
  }

  emit(type: string) {
    for (const listener of this.listeners.get(type) ?? []) {
      listener(new Event(type));
    }
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, reject, resolve };
}

function response(): SessionResponse {
  return {
    snapshot: {} as SessionResponse["snapshot"],
    owner: true,
    can_delete: true,
  };
}

async function flushPromises() {
  await Promise.resolve();
  await Promise.resolve();
}

beforeEach(() => {
  FakeEventSource.instances = [];
  mocks.notify.mockReset();
  mocks.request.mockReset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("watchSessionUpdates", () => {
  it("aborts an in-flight append and ignores its late response on close", async () => {
    const pending = deferred<SessionResponse>();
    mocks.request.mockReturnValueOnce(pending.promise);
    const apply = vi.fn();
    const close = watchSessionUpdates("Ada Lovelace", () => 7, apply);
    const events = FakeEventSource.instances[0];
    if (!events) throw new Error("EventSource was not opened");

    events.emit("update");
    expect(mocks.request).toHaveBeenCalledWith(
      "/api/v1/app/sessions/Ada%20Lovelace/append?after=7",
      { signal: expect.any(AbortSignal) },
    );
    const init = mocks.request.mock.calls[0]?.[1] as RequestInit;
    expect(init.signal?.aborted).toBe(false);

    close();
    expect(init.signal?.aborted).toBe(true);
    expect(events.close).toHaveBeenCalledOnce();
    pending.resolve(response());
    await flushPromises();
    expect(apply).not.toHaveBeenCalled();

    events.emit("update");
    expect(mocks.request).toHaveBeenCalledOnce();
  });

  it("collapses an event burst into one trailing append", async () => {
    const first = deferred<SessionResponse>();
    const trailing = deferred<SessionResponse>();
    mocks.request
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(trailing.promise);
    const apply = vi.fn();
    const close = watchSessionUpdates("1", () => 12, apply);
    const events = FakeEventSource.instances[0];
    if (!events) throw new Error("EventSource was not opened");

    events.emit("update");
    events.emit("update");
    events.emit("update");
    expect(mocks.request).toHaveBeenCalledOnce();

    first.resolve(response());
    await flushPromises();
    expect(mocks.request).toHaveBeenCalledTimes(2);
    trailing.resolve(response());
    await flushPromises();
    expect(apply).toHaveBeenCalledTimes(2);
    expect(mocks.request).toHaveBeenCalledTimes(2);
    close();
  });

  it("reports append failures once and consumes the rejections", async () => {
    mocks.request
      .mockRejectedValueOnce(new Error("offline"))
      .mockRejectedValueOnce(new Error("still offline"));
    const close = watchSessionUpdates("1", () => null, vi.fn());
    const events = FakeEventSource.instances[0];
    if (!events) throw new Error("EventSource was not opened");

    events.emit("update");
    await flushPromises();
    events.emit("update");
    await flushPromises();
    expect(mocks.notify).toHaveBeenCalledOnce();
    expect(mocks.notify).toHaveBeenCalledWith(
      "Live session update failed. Reload the page if updates stop.",
      "err",
    );
    close();
  });
});
