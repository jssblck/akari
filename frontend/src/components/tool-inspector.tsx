// The tool-call inspector: a focus-trapped modal that opens a selected tool call's
// input/output bodies, rendering an edit-family tool's input as a unified diff. It
// is a module-level store (the notices.tsx pattern) rather than prop-drilled state,
// so any tool chip or outline step anywhere in the tree can open it without every
// intermediate component threading a callback. ToolInspectorModal is mounted once
// by Transcript, so both the authenticated and the public session view carry it.
import { useEffect, useRef, useState } from "react";

import { buildDiffView, type DiffView } from "./diff";
import { isDiffTool } from "./session-quality";
import type { ToolCallFull } from "./session-types";

type ViewKey = "diff" | "input" | "result";
type InspectorView = {
  key: ViewKey;
  label: string;
  url: string;
  render: "diff" | "text";
};

export type InspectorDescriptor = {
  tool: string;
  file: string;
  detail: string;
  status: string;
  views: InspectorView[];
  initial: ViewKey;
};

let current: InspectorDescriptor | null = null;
const listeners = new Set<(d: InspectorDescriptor | null) => void>();

function publish() {
  for (const listener of listeners) listener(current);
}

function buildViews(
  inputUrl: string,
  resultUrl: string,
  inputDiff: boolean,
): InspectorView[] {
  const views: InspectorView[] = [];
  if (inputUrl && inputDiff)
    views.push({ key: "diff", label: "Diff", url: inputUrl, render: "diff" });
  if (inputUrl)
    views.push({ key: "input", label: "Input", url: inputUrl, render: "text" });
  if (resultUrl)
    views.push({
      key: "result",
      label: "Output",
      url: resultUrl,
      render: "text",
    });
  return views;
}

// openToolInspector opens the modal for a tool call read off the transcript or the
// outline rail. initialSlot mirrors the old chip behavior: clicking the "in:" stamp
// opens the diff (when the tool is edit-family) or the input body, the "out:" stamp
// opens the result.
export function openToolInspector(
  tool: ToolCallFull,
  blobBase: string,
  initialSlot?: "input" | "result",
) {
  const inputUrl = tool.InputSHA ? `${blobBase}/${tool.InputSHA}` : "";
  const resultUrl = tool.ResultSHA ? `${blobBase}/${tool.ResultSHA}` : "";
  const diff = isDiffTool(tool.ToolName);
  const views = buildViews(inputUrl, resultUrl, diff);
  const firstView = views[0];
  if (!firstView) return;
  let initial: ViewKey =
    initialSlot === "result" ? "result" : diff ? "diff" : "input";
  if (!views.some((v) => v.key === initial)) initial = firstView.key;
  current = {
    tool: tool.ToolName,
    file: tool.FileRelPath || tool.FilePath,
    detail: tool.Detail,
    status: tool.ResultStatus,
    views,
    initial,
  };
  publish();
}

export function closeToolInspector() {
  current = null;
  publish();
}

// BODY_DISPLAY_CAP bounds the text pulled into the page so a huge tool body cannot
// blow up memory; the rest stays one click away as the raw blob link.
const BODY_DISPLAY_CAP = 200_000;

type FetchedBody = { text: string; truncated: boolean; total: number };

// fetchBounded streams the blob and stops once it has the display cap, so peak
// memory tracks the cap rather than the full body size.
async function fetchBounded(url: string, cap: number): Promise<FetchedBody> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) throw new Error(`status ${res.status}`);
  const totalHeader = res.headers.get("Content-Length");
  const total = totalHeader ? Number.parseInt(totalHeader, 10) : -1;
  if (!res.body) {
    const text = await res.text();
    return {
      text: text.slice(0, cap),
      truncated: text.length > cap,
      total: Number.isNaN(total) ? -1 : total,
    };
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let acc = "";
  let truncated = false;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    acc += decoder.decode(value, { stream: true });
    if (acc.length >= cap) {
      truncated = true;
      acc = acc.slice(0, cap);
      await reader.cancel();
      break;
    }
  }
  return { text: acc, truncated, total: Number.isNaN(total) ? -1 : total };
}

// One-entry cache holding only the bounded prefix: re-toggling the same view does
// not refetch, and clicking through many bodies never retains more than one capped
// body at a time.
let lastBody: { url: string; res: FetchedBody | null } = { url: "", res: null };

function InspectorBody({ view }: { view: InspectorView }) {
  const [state, setState] = useState<
    | { kind: "loading" }
    | { kind: "error" }
    | { kind: "ready"; res: FetchedBody }
  >(
    lastBody.url === view.url && lastBody.res
      ? { kind: "ready", res: lastBody.res }
      : { kind: "loading" },
  );

  useEffect(() => {
    if (lastBody.url === view.url && lastBody.res) {
      setState({ kind: "ready", res: lastBody.res });
      return;
    }
    let cancelled = false;
    setState({ kind: "loading" });
    fetchBounded(view.url, BODY_DISPLAY_CAP)
      .then((res) => {
        if (cancelled) return;
        lastBody = { url: view.url, res };
        setState({ kind: "ready", res });
      })
      .catch(() => {
        if (!cancelled) setState({ kind: "error" });
      });
    return () => {
      cancelled = true;
    };
  }, [view.url]);

  if (state.kind === "loading")
    return <div className="insp-loading muted">Loading…</div>;
  if (state.kind === "error")
    return <pre className="tool-body error">Could not load body.</pre>;

  const { res } = state;
  let diff: DiffView | null = null;
  if (view.render === "diff" && !res.truncated) diff = buildDiffView(res.text);

  return (
    <>
      {diff ? (
        <div className="diff">
          {diff.file ? <div className="diff-file">{diff.file}</div> : null}
          <pre className="diff-body">
            {diff.lines.map((line, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: diff lines carry no stable identity; the view never reorders in place.
              <span className={`diff-line diff-${line.kind}`} key={i}>
                {line.text}
              </span>
            ))}
          </pre>
        </div>
      ) : res.text ? (
        <pre className="tool-body">{res.text}</pre>
      ) : null}
      {res.truncated ? (
        <div className="insp-trunc muted">
          Showing the first {Math.round(BODY_DISPLAY_CAP / 1000)} KB
          {res.total > 0 ? ` of ${Math.round(res.total / 1000)} KB` : ""}.{" "}
          <a href={view.url} target="_blank" rel="noreferrer">
            Open raw
          </a>
        </div>
      ) : null}
    </>
  );
}

export function ToolInspectorModal() {
  const [desc, setDesc] = useState<InspectorDescriptor | null>(current);
  const [activeKey, setActiveKey] = useState<ViewKey | null>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const lastFocused = useRef<HTMLElement | null>(null);

  useEffect(() => {
    listeners.add(setDesc);
    return () => {
      listeners.delete(setDesc);
    };
  }, []);

  useEffect(() => {
    if (!desc) {
      setActiveKey(null);
      return;
    }
    setActiveKey(desc.initial);
    lastFocused.current = document.activeElement as HTMLElement | null;
    document.body.classList.add("modal-open");
    dialogRef.current?.focus();
    return () => {
      document.body.classList.remove("modal-open");
    };
  }, [desc]);

  useEffect(() => {
    if (!desc) return;
    function onKeyDown(ev: KeyboardEvent) {
      if (ev.key === "Escape") {
        closeToolInspector();
        return;
      }
      if (ev.key !== "Tab") return;
      const dialog = dialogRef.current;
      if (!dialog) return;
      const focusables = Array.from(
        dialog.querySelectorAll<HTMLElement>(
          'a[href], button, [tabindex]:not([tabindex="-1"])',
        ),
      ).filter(
        (el) =>
          !("disabled" in el && (el as HTMLButtonElement).disabled) &&
          el.offsetParent !== null,
      );
      if (focusables.length === 0) {
        ev.preventDefault();
        return;
      }
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (!first || !last) return;
      const idx = focusables.indexOf(document.activeElement as HTMLElement);
      if (ev.shiftKey && idx <= 0) {
        ev.preventDefault();
        last.focus();
      } else if (
        !ev.shiftKey &&
        (idx === -1 || idx === focusables.length - 1)
      ) {
        ev.preventDefault();
        first.focus();
      }
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [desc]);

  function handleClose() {
    closeToolInspector();
    if (lastFocused.current?.isConnected) lastFocused.current.focus();
  }

  const activeView =
    desc?.views.find((v) => v.key === activeKey) ?? desc?.views[0];

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: backdrop click-to-close is a mouse-only convenience; Escape (handled above) is the full keyboard equivalent.
    // biome-ignore lint/a11y/useKeyWithClickEvents: same as above, Escape already closes the dialog.
    <div
      className="modal-overlay"
      hidden={!desc}
      onClick={(ev) => {
        if (ev.target === ev.currentTarget) handleClose();
      }}
    >
      {desc ? (
        <div
          className="modal-dialog"
          role="dialog"
          aria-modal="true"
          aria-label="Tool call body"
          tabIndex={-1}
          ref={dialogRef}
        >
          <div className="insp-head">
            <span className="insp-tn">{desc.tool}</span>
            {desc.file ? (
              <span className="insp-file mono">{desc.file}</span>
            ) : null}
            {desc.detail ? (
              <span className="insp-detail mono">{desc.detail}</span>
            ) : null}
            <span className="insp-spacer" />
            {desc.status ? (
              <span
                className={`insp-status tstatus ${desc.status === "error" ? "err" : "ok"}`}
              >
                {desc.status}
              </span>
            ) : null}
            <button
              type="button"
              className="insp-close"
              aria-label="Close"
              onClick={handleClose}
            >
              &times;
            </button>
          </div>
          <div className="seg-group insp-views">
            {desc.views.map((v) => (
              <button
                type="button"
                key={v.key}
                className={`seg${v.key === activeKey ? " active" : ""}`}
                onClick={() => setActiveKey(v.key)}
              >
                {v.label}
              </button>
            ))}
          </div>
          <div className="insp-body">
            {activeView ? <InspectorBody view={activeView} /> : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
