import { useEffect, useState } from "react";

import { withBase } from "./base";

let csrfToken = "";

export function setCSRFToken(token: string | undefined) {
  csrfToken = token ?? "";
}

// FleetStatus mirrors parse.Status: the server's rebuild-on-dirty worker
// reports it verbatim (json tags in_progress/done/total/failed/started_at)
// from the reparse status endpoint, the reparse SSE stream, and the 503 gate
// body every parsed-data endpoint returns while a fleet rebuild is draining.
export type FleetStatus = {
  in_progress: boolean;
  done: number;
  total: number;
  failed: number;
  started_at?: string;
};

// ProblemResponse is the JSON shape every failed request can return: a plain
// error string, or (only from a gated endpoint) that plus the fleet status so
// the caller can tell a rebuild-in-progress 503 apart from a real failure.
type ProblemResponse = { error?: string; reparse?: unknown };

function asFleetStatus(value: unknown): FleetStatus | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  if (!("in_progress" in value)) return undefined;
  return value as FleetStatus;
}

export class RequestError extends Error {
  readonly status: number;
  readonly retryAfterMs: number;
  // Set only when the server rejected the request because a fleet reparse is
  // draining (a 503 whose body carries a reparse status). useAPI polls on
  // this rather than surfacing the failure, so the view self-heals once the
  // rebuild finishes.
  readonly reparse?: FleetStatus | undefined;

  constructor(
    status: number,
    message: string,
    reparse?: FleetStatus,
    retryAfterMs = 0,
  ) {
    super(message);
    this.name = "RequestError";
    this.status = status;
    this.reparse = reparse;
    this.retryAfterMs = retryAfterMs;
  }
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body && !headers.has("Content-Type"))
    headers.set("Content-Type", "application/json");
  if (
    csrfToken &&
    init?.method &&
    !["GET", "HEAD", "OPTIONS"].includes(init.method)
  ) {
    headers.set("X-Akari-CSRF-Token", csrfToken);
  }
  const response = await fetch(withBase(path), {
    ...init,
    headers,
    credentials: "same-origin",
  });
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    let reparse: FleetStatus | undefined;
    try {
      const problem: ProblemResponse = await response.json();
      if (problem.error) message = problem.error;
      reparse = asFleetStatus(problem.reparse);
    } catch {
      // The status line is the useful fallback for non-JSON failures.
    }
    throw new RequestError(
      response.status,
      message,
      reparse,
      parseRetryAfter(response.headers.get("Retry-After")),
    );
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

type Wait = (delayMs: number, signal?: AbortSignal) => Promise<void>;

export const waitForRetry: Wait = (delayMs, signal) =>
  new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException("Aborted", "AbortError"));
      return;
    }
    const onAbort = () => {
      globalThis.clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    };
    const timer = globalThis.setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, delayMs);
    signal?.addEventListener("abort", onAbort, { once: true });
  });

// Direct transcript-page reads sit outside useAPI, but they cross the same
// rebuild gate. Retrying only errors that carry fleet status keeps transient
// projection work invisible without turning unrelated 503s into an endless loop.
export async function requestWithRetry<T>(
  path: string,
  init: RequestInit = {},
  waitForRetryDelay: Wait = waitForRetry,
): Promise<T> {
  for (;;) {
    try {
      return await request<T>(path, init);
    } catch (error) {
      if (
        !(error instanceof RequestError) ||
        error.status !== 503 ||
        !error.reparse
      )
        throw error;
      await waitForRetryDelay(
        error.retryAfterMs || REPARSE_POLL_MS,
        init.signal ?? undefined,
      );
    }
  }
}

export function parseRetryAfter(value: string | null): number {
  if (!value) return 0;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return seconds * 1_000;
  const at = Date.parse(value);
  return Number.isNaN(at) ? 0 : Math.max(0, at - Date.now());
}

// Reparse-gate polling stays slow and constant: the worker's own status
// endpoint is meant for exactly this kind of coarse "is it still draining"
// check, and a fixed interval avoids building a backoff scheme for a state
// that, per the account page's live SSE view, is usually resolved in seconds
// to minutes.
const REPARSE_POLL_MS = 5_000;

export type LoadState<T> =
  | { kind: "loading" }
  | { kind: "error"; error: Error }
  | { kind: "gated"; reparse: FleetStatus }
  | { kind: "ready"; data: T };

export function useAPI<T>(path: string): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ kind: "loading" });

  useEffect(() => {
    let cancelled = false;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();

    const load = () => {
      request<T>(path, { signal: controller.signal })
        .then((data) => {
          if (!cancelled) setState({ kind: "ready", data });
        })
        .catch((error: unknown) => {
          if (cancelled) return;
          if (error instanceof DOMException && error.name === "AbortError")
            return;
          if (
            error instanceof RequestError &&
            error.status === 503 &&
            error.reparse
          ) {
            // Stay in the gated state (rather than bouncing through
            // "loading") so a view already showing the quiet rebuilding note
            // does not flash a skeleton between polls.
            setState({ kind: "gated", reparse: error.reparse });
            retryTimer = setTimeout(load, REPARSE_POLL_MS);
            return;
          }
          setState({
            kind: "error",
            error: error instanceof Error ? error : new Error("Request failed"),
          });
        });
    };

    // Stale-while-revalidate: when the path changes on a mounted view (a
    // range or filter tweak), keep showing the last payload until the new one
    // lands instead of collapsing to the loading skeleton, which reads as a
    // full page reload. Only the first load has no data to hold onto.
    setState((prev) => (prev.kind === "ready" ? prev : { kind: "loading" }));
    load();

    return () => {
      cancelled = true;
      controller.abort();
      if (retryTimer) clearTimeout(retryTimer);
    };
  }, [path]);

  return state;
}
