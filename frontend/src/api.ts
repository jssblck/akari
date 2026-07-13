import { useEffect, useState } from "react";

import type { APIError } from "./types";

let csrfToken = "";

export function setCSRFToken(token: string | undefined) {
  csrfToken = token ?? "";
}

export class RequestError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "RequestError";
    this.status = status;
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
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: "same-origin",
  });
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    try {
      const problem: APIError = await response.json();
      if (problem.error) message = problem.error;
    } catch {
      // The status line is the useful fallback for non-JSON failures.
    }
    throw new RequestError(response.status, message);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export type LoadState<T> =
  | { kind: "loading" }
  | { kind: "error"; error: Error }
  | { kind: "ready"; data: T };

export function useAPI<T>(path: string): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    request<T>(path, { signal: controller.signal })
      .then((data) => setState({ kind: "ready", data }))
      .catch((error: unknown) => {
        if (error instanceof DOMException && error.name === "AbortError")
          return;
        setState({
          kind: "error",
          error: error instanceof Error ? error : new Error("Request failed"),
        });
      });
    return () => controller.abort();
  }, [path]);

  return state;
}
