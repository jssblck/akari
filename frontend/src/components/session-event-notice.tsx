import { formatTokens } from "../format";
import type { SessionEvent } from "../types";

function textAttr(attrs: SessionEvent["Attrs"], key: string): string {
  const value = attrs[key];
  return typeof value === "string" ? value : "";
}

function numberAttr(attrs: SessionEvent["Attrs"], key: string): number | null {
  const value = attrs[key];
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function errorsAttr(attrs: SessionEvent["Attrs"]): string[] {
  const value = attrs.errors;
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}

export function sessionEventLabel(event: SessionEvent): string | null {
  const attrs = event.Attrs;
  switch (event.Kind) {
    case "compaction": {
      const parts = ["context compacted"];
      const trigger = textAttr(attrs, "trigger");
      if (trigger) parts.push(trigger);
      const pre = numberAttr(attrs, "pre_tokens");
      const post = numberAttr(attrs, "post_tokens");
      const dropped = numberAttr(attrs, "dropped_tokens");
      if (pre !== null && post !== null) {
        parts.push(`${formatTokens(pre)} to ${formatTokens(post)}`);
      }
      if (dropped !== null) parts.push(`${formatTokens(dropped)} dropped`);
      return parts.join(" / ");
    }
    case "turn_aborted": {
      const reason = textAttr(attrs, "reason");
      return reason ? `turn aborted / ${reason}` : "turn aborted";
    }
    case "api_error": {
      const message = textAttr(attrs, "message");
      return message ? `API error / ${message}` : "API error";
    }
    case "model_change": {
      const provider = textAttr(attrs, "provider");
      const model = textAttr(attrs, "model");
      const target = [provider, model].filter(Boolean).join("/");
      return target ? `model changed / ${target}` : "model changed";
    }
    case "thinking_level_change": {
      const level = textAttr(attrs, "level");
      return level
        ? `thinking level changed / ${level}`
        : "thinking level changed";
    }
    case "stop_hook": {
      const prevented = attrs.prevented_continuation === true;
      const errors = errorsAttr(attrs);
      if (!prevented && errors.length === 0) return null;
      const parts = [
        prevented ? "stop hook prevented continuation" : "stop hook error",
      ];
      const reason = textAttr(attrs, "stop_reason");
      if (reason) parts.push(reason);
      if (errors.length > 0) parts.push(errors.join("; "));
      return parts.join(" / ");
    }
    default:
      return null;
  }
}

export function SessionEventNotice({ event }: { event: SessionEvent }) {
  const label = sessionEventLabel(event);
  if (!label) return null;
  return (
    <div className="msg-event" role="note">
      <span className="event-label">{label}</span>
    </div>
  );
}
