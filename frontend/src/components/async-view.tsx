import type { ReactNode } from "react";

import type { LoadState } from "../api";
import "./async-view.css";

export function AsyncView<T>({
  state,
  children,
  renderError,
}: {
  state: LoadState<T>;
  children: (data: T) => ReactNode;
  // Lets a page swap the generic "could not load" card for its own error
  // treatment (the public pages need a "go home" link for a bad or
  // unpublished id, not a retry button that would just fail again).
  renderError?: (error: Error) => ReactNode;
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
    case "gated":
      // A fleet reparse is draining and this endpoint refuses to mix
      // pre/post-rebuild sessions. useAPI is already polling behind the
      // scenes, so this reads as a quiet wait rather than a failure.
      return (
        <div className="skeleton-stack" role="status" aria-live="polite">
          <p className="gated-note">
            Rebuilding session data ({state.reparse.done} /{" "}
            {state.reparse.total}
            ). This view refreshes automatically.
          </p>
        </div>
      );
    case "error":
      if (renderError) return <>{renderError(state.error)}</>;
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
