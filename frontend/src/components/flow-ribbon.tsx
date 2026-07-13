// The flow ribbon: one tick per turn, colored by what the turn did, so a reviewer
// sees a session's shape (a failure streak, an edit-churn loop) before reading a
// word. Ported from web.flowRibbon / web.FlowTickClass (internal/server/web/flow.go).
// The ticks are flex items with a capped max-width (see sessions.css), the fix from
// "Fix dense turn minimap overflow" (46441c1): a dense session's ticks shrink
// together instead of overflowing the strip.
import type { Message, ToolCall } from "../types";
import { outlineTitle } from "./session-quality";

const FLOW_RIBBON_MIN_MESSAGES = 12;

function tickClass(m: Message, steps: ToolCall[]): string {
  let edit = false;
  let run = false;
  for (const t of steps) {
    if (t.ResultStatus === "error") return "ft-fail";
    if (t.Category === "edit" || t.Category === "write") edit = true;
    if (t.Category === "bash") run = true;
  }
  if (edit) return "ft-edit";
  if (run) return "ft-run";
  if (m.Role === "user") return "ft-user";
  return "ft-plain";
}

function tickTitle(m: Message, steps: ToolCall[]): string {
  let label = `#${m.Ordinal} ${m.Role}`;
  if (m.Role !== "assistant") {
    const title = outlineTitle(m.Role, m.Content);
    if (title) label += `: ${title}`;
  }
  if (steps.length > 0) {
    const failed = steps.filter((s) => s.ResultStatus === "error").length;
    label += ` · ${steps.length} tools`;
    if (failed > 0) label += `, ${failed} failed`;
  }
  return label;
}

export function FlowRibbon({
  outline,
  toolsByOrdinal,
}: {
  outline: Message[];
  toolsByOrdinal: Map<number, ToolCall[]>;
}) {
  if (outline.length < FLOW_RIBBON_MIN_MESSAGES) return null;
  return (
    <nav
      className="flow"
      aria-label="Session flow: one tick per turn, colored by activity"
    >
      {outline.map((m) => {
        const steps = toolsByOrdinal.get(m.Ordinal) ?? [];
        const label = tickTitle(m, steps);
        return (
          <a
            key={m.Ordinal}
            className={`flow-tick ${tickClass(m, steps)}`}
            href={`#msg-${m.Ordinal}`}
            title={label}
            data-ord={m.Ordinal}
          >
            <span className="sr-only">{label}</span>
          </a>
        );
      })}
    </nav>
  );
}
