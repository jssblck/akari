import { request } from "../api";
import { withBase } from "../base";
import { notify } from "../components/notices";
import type { SessionResponse } from "../types";

// watchSessionUpdates keeps one append request in flight and collapses an SSE
// burst into one trailing request, so older responses cannot overwrite newer
// session state. Closing the watcher aborts the request and makes a transport
// that ignores abort harmless by discarding its late response.
export function watchSessionUpdates(
  id: string,
  lastOrdinal: () => number | null,
  apply: (result: SessionResponse, after: number | null) => void,
): () => void {
  const controller = new AbortController();
  const events = new EventSource(
    withBase(`/sessions/${encodeURIComponent(id)}/events`),
  );
  let active = true;
  let fetching = false;
  let pending = false;
  let failureReported = false;

  async function refresh() {
    if (!active) return;
    if (fetching) {
      pending = true;
      return;
    }
    fetching = true;
    try {
      const after = lastOrdinal();
      const query = after == null ? "0" : String(after);
      const result = await request<SessionResponse>(
        `/api/v1/app/sessions/${encodeURIComponent(id)}/append?after=${query}`,
        { signal: controller.signal },
      );
      if (active) apply(result, after);
    } catch {
      if (active && !controller.signal.aborted && !failureReported) {
        failureReported = true;
        notify(
          "Live session update failed. Reload the page if updates stop.",
          "err",
        );
      }
    } finally {
      fetching = false;
      if (active && pending) {
        pending = false;
        void refresh();
      }
    }
  }

  const onUpdate = () => void refresh();
  events.addEventListener("update", onUpdate);
  return () => {
    active = false;
    pending = false;
    controller.abort();
    events.removeEventListener("update", onUpdate);
    events.close();
  };
}
