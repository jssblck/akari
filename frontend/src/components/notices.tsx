import { useEffect, useState } from "react";

import { RequestError } from "../api";

export type NoticeKind = "ok" | "err";
export type Notice = { id: number; kind: NoticeKind; message: string };

// A module-level store instead of React context so any code (mutation
// handlers, api helpers) can raise a notice without threading a hook through
// every call site. NoticeHost subscribes and renders whatever is queued.
let nextID = 1;
let queue: Notice[] = [];
const listeners = new Set<(notices: Notice[]) => void>();

function publish() {
  for (const listener of listeners) listener(queue);
}

export function dismissNotice(id: number) {
  queue = queue.filter((notice) => notice.id !== id);
  publish();
}

export function notify(message: string, kind: NoticeKind = "ok") {
  const id = nextID++;
  queue = [...queue, { id, kind, message }];
  publish();
  // Confirmations expire on their own; errors stay until dismissed so a
  // failure can't vanish before it is read.
  if (kind === "ok") setTimeout(() => dismissNotice(id), 5000);
  return id;
}

// attempt wraps a mutation: on success it optionally confirms, on failure it
// surfaces the server's error instead of letting the rejection vanish. Returns
// whether the action succeeded so callers can skip their refresh on failure.
export async function attempt(
  action: Promise<unknown>,
  successMessage?: string,
): Promise<boolean> {
  try {
    await action;
    if (successMessage) notify(successMessage, "ok");
    return true;
  } catch (error) {
    const message =
      error instanceof RequestError
        ? error.message
        : "Request failed; check your connection and try again.";
    notify(message, "err");
    return false;
  }
}

export function NoticeHost() {
  const [notices, setNotices] = useState<Notice[]>(queue);
  useEffect(() => {
    listeners.add(setNotices);
    return () => {
      listeners.delete(setNotices);
    };
  }, []);
  if (notices.length === 0) return null;
  return (
    <div className="notice-stack" role="status" aria-live="polite">
      {notices.map((notice) => (
        <div className={`notice ${notice.kind}`} key={notice.id}>
          <span>{notice.message}</span>
          <button
            type="button"
            aria-label="Dismiss"
            onClick={() => dismissNotice(notice.id)}
          >
            &times;
          </button>
        </div>
      ))}
    </div>
  );
}
