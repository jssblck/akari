// The outline rail: the session's left pane, one entry per turn with its tool
// steps nested beneath, scroll-spied so the entry under the reading line stays
// highlighted. Ported from session.templ's Outline and app.js's initOutlineSpy.
import { useEffect } from "react";

import type { Message, ToolCall } from "../types";
import {
  baseName,
  detailLabel,
  outlineTitle,
  toolFilePath,
  toolFileTitle,
} from "./session-quality";
import { openToolInspector } from "./tool-inspector";

function turnClass(role: string, steps: ToolCall[]): string {
  if (steps.some((t) => t.ResultStatus === "error")) return "ol-turn ol-error";
  switch (role) {
    case "user":
      return "ol-turn ol-user";
    case "assistant":
      return "ol-turn ol-assistant";
    case "context":
      return "ol-turn ol-context";
    default:
      return "ol-turn ol-other";
  }
}

function stepClass(t: ToolCall): string {
  return t.ResultStatus === "error" ? "ol-step ol-step-error" : "ol-step";
}

function hasBody(t: ToolCall): boolean {
  return Boolean(t.InputSHA || t.ResultSHA);
}

// useOutlineScrollSpy highlights the outline turn whose message sits at the reading
// line: it samples the one message under a fixed point (rAF-throttled), so the cost
// is O(1) per scroll tick rather than growing with the session. It re-runs whenever
// the outline's own entries change (a live append adds rows), matching the old
// code's "keeps working across live transcript swaps" behavior.
function useOutlineScrollSpy(dep: number) {
  // biome-ignore lint/correctness/useExhaustiveDependencies: dep is a change counter (outline length), not a value read in the effect body.
  useEffect(() => {
    let current: Element | null = null;
    let ticking = false;
    function update() {
      ticking = false;
      const transcript = document.querySelector(".transcript");
      if (!transcript) return;
      const rect = transcript.getBoundingClientRect();
      const el = document.elementFromPoint(
        rect.left + Math.min(rect.width / 2, 180),
        window.innerHeight * 0.32,
      );
      const msg = el?.closest("[data-ordinal]");
      if (!msg) return;
      const entry = document.getElementById(
        `ol-${msg.getAttribute("data-ordinal")}`,
      );
      if (!entry || entry === current) return;
      current?.classList.remove("current");
      entry.classList.add("current");
      current = entry;
    }
    function onScroll() {
      if (!ticking) {
        ticking = true;
        requestAnimationFrame(update);
      }
    }
    window.addEventListener("scroll", onScroll, { passive: true });
    update();
    return () => window.removeEventListener("scroll", onScroll);
  }, [dep]);
}

export function OutlineRail({
  outline,
  toolsByOrdinal,
  blobBase,
}: {
  outline: Message[];
  toolsByOrdinal: Map<number, ToolCall[]>;
  blobBase: string;
}) {
  useOutlineScrollSpy(outline.length);
  if (outline.length === 0) return null;
  return (
    <aside className="outline-rail" aria-label="Outline">
      <div className="outline">
        <div className="label outline-head">Outline</div>
        {outline.map((m) => {
          const steps = toolsByOrdinal.get(m.Ordinal) ?? [];
          const title = outlineTitle(m.Role, m.Content);
          return (
            <div className="ol-group" key={m.Ordinal}>
              <a
                className={turnClass(m.Role, steps)}
                id={`ol-${m.Ordinal}`}
                href={`#msg-${m.Ordinal}`}
                data-ord={m.Ordinal}
              >
                <span className="ol-dot" />
                <span className="ol-title">
                  {title ? title : <span className="ol-role">{m.Role}</span>}
                </span>
                {steps.length > 0 ? (
                  <span className="ol-count">{steps.length}</span>
                ) : null}
              </a>
              {steps.length > 0 ? (
                <div className="ol-steps">
                  {steps.map((s) =>
                    hasBody(s) ? (
                      <a
                        key={`${m.Ordinal}-${s.CallIndex}`}
                        className={`${stepClass(s)} inspect-open`}
                        href={`#msg-${m.Ordinal}`}
                        onClick={(ev) => {
                          ev.preventDefault();
                          openToolInspector(s, blobBase);
                        }}
                      >
                        <span className="ol-tn">{s.ToolName}</span>
                        {toolFilePath(s) ? (
                          <span className="ol-file" title={toolFileTitle(s)}>
                            {baseName(toolFilePath(s))}
                          </span>
                        ) : null}
                        {s.Detail ? (
                          <span className="ol-detail" title={s.Detail}>
                            {detailLabel(s.Detail)}
                          </span>
                        ) : null}
                      </a>
                    ) : (
                      <a
                        key={`${m.Ordinal}-${s.CallIndex}`}
                        className={stepClass(s)}
                        href={`#msg-${m.Ordinal}`}
                      >
                        <span className="ol-tn">{s.ToolName}</span>
                        {s.Detail ? (
                          <span className="ol-detail" title={s.Detail}>
                            {detailLabel(s.Detail)}
                          </span>
                        ) : null}
                      </a>
                    ),
                  )}
                </div>
              ) : null}
            </div>
          );
        })}
      </div>
    </aside>
  );
}
