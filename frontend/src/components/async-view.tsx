import type { ReactNode } from "react";

import type { LoadState } from "../api";

export function AsyncView<T>({
  state,
  children,
}: {
  state: LoadState<T>;
  children: (data: T) => ReactNode;
}) {
  switch (state.kind) {
    case "loading":
      return (
        <div className="skeleton-stack" role="status" aria-label="Loading">
          <span />
          <span />
          <span />
        </div>
      );
    case "error":
      return (
        <section className="empty-state" role="alert">
          <h2>Could not load this view</h2>
          <p>{state.error.message}</p>
          <button
            type="button"
            className="button secondary"
            onClick={() => window.location.reload()}
          >
            Try again
          </button>
        </section>
      );
    case "ready":
      return children(state.data);
  }
}
