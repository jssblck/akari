// Pure view-model helpers ported from internal/quality and internal/server/web, so
// the session list and session detail pages can derive the same bands, labels, and
// score arithmetic the Go renderer used to, without a server round trip. Each
// function names the Go source it mirrors; keep them in step by hand since there is
// no shared source of truth across the language boundary.

import { formatTokens } from "../format";
import type {
  ModelFallback,
  SessionSignals,
  TurnUsageFull,
} from "./session-types";

// quality.GradeBand: buckets a letter grade into the tier the UI colors by.
export function gradeBand(
  grade: string | null,
): "good" | "watch" | "poor" | "none" {
  switch (grade) {
    case "A":
    case "B":
      return "good";
    case "C":
      return "watch";
    case "D":
    case "F":
      return "poor";
    default:
      return "none";
  }
}

// web.QualityGrade / web.QualityScoreLabel: the tile headline and its tooltip line.
export function isScored(s: SessionSignals): boolean {
  return s.Score !== null && s.Grade !== null;
}

export function qualityGrade(s: SessionSignals): string {
  return isScored(s) && s.Grade ? s.Grade : "-";
}

export function qualityScoreLabel(s: SessionSignals): string {
  return isScored(s) ? `${s.Score} / 100` : "not scored";
}

// web.OutcomeLabel
export function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "completed":
      return "Completed";
    case "abandoned":
      return "Abandoned";
    case "errored":
      return "Errored";
    default:
      return "Unknown";
  }
}

// web.RowOutcomeNote: a short outcome word worth flagging on a feed row, empty for
// the outcomes that need no glance (completed, unknown).
export function rowOutcomeNote(outcome: string): string {
  if (outcome === "abandoned") return "abandoned";
  if (outcome === "errored") return "errored";
  return "";
}

// quality.ScoreBreakdown: the penalty lines behind a scored session's grade, in the
// same order and with the same caps Score applies, so the tooltip's arithmetic sums
// to the same 100-minus-score the Go scorer would report.
const penErrored = 30;
const penAbandoned = 15;
const penPerFailure = 3;
const capFailures = 30;
const penPerRetry = 5;
const capRetries = 25;
const penPerChurn = 4;
const capChurn = 20;
const penFailureStreak = 10;
const failureStreakFloor = 3;

function plural(n: number, singular: string): string {
  const noun =
    n === 1
      ? singular
      : singular.endsWith("y")
        ? `${singular.slice(0, -1)}ies`
        : `${singular}s`;
  return `${n} ${noun}`;
}

export type ScoreBreakdownItem = { label: string; points: number };

export function scoreBreakdownItems(s: SessionSignals): ScoreBreakdownItem[] {
  const hasToolSignal =
    s.ToolFailures > 0 ||
    s.ToolRetries > 0 ||
    s.EditChurn > 0 ||
    s.LongestFailureStreak > 0;
  if (s.Outcome === "unknown" && !hasToolSignal) return [];
  const items: ScoreBreakdownItem[] = [];
  if (s.Outcome === "errored")
    items.push({ label: "errored ending", points: penErrored });
  else if (s.Outcome === "abandoned")
    items.push({ label: "abandoned", points: penAbandoned });
  const failures = Math.min(s.ToolFailures * penPerFailure, capFailures);
  if (failures > 0)
    items.push({
      label: plural(s.ToolFailures, "tool failure"),
      points: failures,
    });
  const retries = Math.min(s.ToolRetries * penPerRetry, capRetries);
  if (retries > 0)
    items.push({ label: plural(s.ToolRetries, "retry"), points: retries });
  const churn = Math.min(s.EditChurn * penPerChurn, capChurn);
  if (churn > 0)
    items.push({ label: plural(s.EditChurn, "churned edit"), points: churn });
  if (s.LongestFailureStreak >= failureStreakFloor)
    items.push({ label: "failure streak", points: penFailureStreak });
  return items;
}

export function hasHygieneSignal(s: SessionSignals): boolean {
  return (
    s.ShortPromptCount > 0 ||
    s.DuplicatePromptCount > 0 ||
    s.NoCodeContextCount > 0 ||
    s.UnstructuredStart
  );
}

// quality.ThinkingBucket: the absolute band an observed-thinking volume lands in.
export type ThinkingBucket = "off" | "low" | "medium" | "high" | "xhigh";

const thinkingLowMax = 128;
const thinkingMediumMax = 512;
const thinkingHighMax = 2048;

// quality.ThinkingBucketForTokens
export function thinkingBucketForTokens(tokens: number): ThinkingBucket {
  if (tokens <= thinkingLowMax) return "low";
  if (tokens <= thinkingMediumMax) return "medium";
  if (tokens <= thinkingHighMax) return "high";
  return "xhigh";
}

// web.ThinkingBucketLabel
export function thinkingBucketLabel(b: ThinkingBucket): string {
  return b === "xhigh" ? "very high" : b;
}

// web.ThinkingTokensLabel
export function thinkingTokensLabel(tokens: number): string {
  return `~${formatTokens(tokens)} tok`;
}

// quality.ThinkingBytesPerToken: the agent-specific bytes-per-reasoning-token
// divisor, defaulting to Claude's factor (the common encrypted-signature case).
const CLAUDE_THINKING_BYTES_PER_TOKEN = 10.7;
const thinkingBytesPerToken: Record<string, number> = {
  claude: CLAUDE_THINKING_BYTES_PER_TOKEN,
  codex: 14.2,
  pi: 4.0,
};

export function thinkingBytesPerTokenFor(agent: string): number {
  return thinkingBytesPerToken[agent] ?? CLAUDE_THINKING_BYTES_PER_TOKEN;
}

// web.MessageThinkingBand: the per-turn thinking chip's band, for one assistant
// turn that carried a reasoning block. Returns null for a turn with none, so the
// caller renders no chip.
export function messageThinkingBand(
  agent: string,
  hasThinking: boolean,
  thinkingBytes: number,
  usageReasoning: number,
): ThinkingBucket | null {
  if (!hasThinking) return null;
  const tokens =
    usageReasoning > 0
      ? usageReasoning
      : thinkingBytes / thinkingBytesPerTokenFor(agent);
  return thinkingBucketForTokens(Math.round(tokens));
}

// quality.IsContextReset: a context reset (compaction or clear) fires when this
// turn's occupancy falls to at most half the prior turn's, and the prior turn was
// already at least 20k tokens.
const resetDropFraction = 0.5;
const resetKeepFloorTokens = 20000;

export function isContextReset(prev: number, cur: number): boolean {
  return prev >= resetKeepFloorTokens && cur <= prev * resetDropFraction;
}

// web.TurnTokenTotal / web.TurnCostLabel / web.FmtContextStamp / web.ShedLabel
export function turnTokenTotal(u: TurnUsageFull): number {
  return u.Input + u.Output + u.CacheRead + u.CacheWrite;
}

export function turnCostLabel(
  u: TurnUsageFull,
  formatCost: (v: number, incomplete?: boolean) => string,
): string {
  return u.CostUSD === null || u.CostUSD === undefined
    ? "unpriced"
    : formatCost(u.CostUSD, u.CostIncomplete);
}

export function contextStamp(u: TurnUsageFull): string {
  return `ctx ${formatTokens(u.ContextTokens)}`;
}

export function shedLabel(fromTokens: number, toTokens: number): string {
  return `context shed: ${formatTokens(fromTokens)} → ${formatTokens(toTokens)}`;
}

// web.FmtLatency / web.FmtTurnLatency
export function formatLatency(ms: number): string {
  const secs = ms / 1000;
  if (secs <= 0) return "-";
  if (secs < 1) return "<1s";
  const whole = Math.round(secs);
  if (whole < 60) return `${whole}s`;
  if (whole < 3600) return `${Math.floor(whole / 60)}m ${whole % 60}s`;
  return `${Math.floor(whole / 3600)}h ${Math.floor((whole % 3600) / 60)}m`;
}

export function turnLatency(ms: number): string {
  return `+${formatLatency(ms)}`;
}

// durationfmt.Positive / durationfmt.Span
export function formatDurationSpan(
  start: string | null,
  end: string | null,
): string {
  if (!start || !end) return "-";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (ms < 0) return "-";
  return formatDurationPositive(ms);
}

export function formatDurationPositive(ms: number): string {
  const secs = Math.floor(ms / 1000);
  if (ms >= 3_600_000)
    return `${Math.floor(secs / 3600)}h${Math.floor((secs % 3600) / 60)}m`;
  if (ms >= 60_000) return `${Math.floor(secs / 60)}m${secs % 60}s`;
  return `${secs}s`;
}

// web.FallbackBadgeLabel / web.FallbackBadgeTitle
export function fallbackBadgeLabel(count: number): string {
  return count <= 1 ? "fallback" : `fallback ×${count}`;
}

export function fallbackBadgeTitle(count: number): string {
  return count <= 1
    ? "1 turn fell back from Fable 5 to a lower model"
    : `${count} turns fell back from Fable 5 to a lower model`;
}

// web.FallbackModelsLabel / FallbackCategoryLabel / FallbackNoticeLabel
export function fallbackModelsLabel(f: ModelFallback): string {
  return `${f.FromModel || "unknown"} → ${f.ToModel || "unknown"}`;
}

export function fallbackCategoryLabel(f: ModelFallback): string {
  return f.RefusalCategory || "uncategorized";
}

export function fallbackDeclinedObserved(f: ModelFallback): boolean {
  return (
    f.DeclinedInput !== null &&
    f.DeclinedOutput !== null &&
    f.DeclinedCacheWrite !== null &&
    f.DeclinedCacheRead !== null
  );
}

export function fallbackNoticeLabel(f: ModelFallback): string {
  const from = f.FromModel || "unknown";
  const to = f.ToModel || "unknown";
  const reason = f.RefusalCategory || f.Trigger;
  return reason
    ? `Fell back from ${from} to ${to} (${reason})`
    : `Fell back from ${from} to ${to}`;
}

// web.ContextLabel: names what an injected-context turn carries, from the marker
// its content opens with.
export function contextLabel(content: string): string {
  const t = content.trim();
  const hasAgents =
    t.startsWith("# AGENTS.md instructions for ") ||
    t.startsWith("<user_instructions>");
  const hasEnv = t.includes("<environment_context>");
  if (hasAgents && hasEnv) return "project instructions + environment";
  if (hasAgents) return "project instructions";
  if (hasEnv) return "environment";
  return "agent context";
}

// web.DiffTool: the file-editing tools worth rendering as an inline diff.
const diffToolNames = new Set([
  "edit",
  "write",
  "multiedit",
  "apply_patch",
  "str_replace",
  "str_replace_editor",
  "create_file",
  "update_file",
]);

export function isDiffTool(name: string): boolean {
  return diffToolNames.has(name.toLowerCase());
}

// web.OutlineTitle: a compact one-line label for an outline turn or flow-tick title.
export function outlineTitle(role: string, content: string): string {
  if (role === "context") return contextLabel(content);
  const collapsed = content.replace(/\s+/g, " ").trim();
  const max = 48;
  if (collapsed.length <= max) return collapsed;
  return `${collapsed.slice(0, max)}…`;
}

// web.DetailLabel: collapses whitespace and caps a tool call's Detail (a command,
// pattern, or URL) at 80 runes with a trailing ellipsis, so a chip or outline step
// stays one scannable line; the full text still reaches the reader through the
// element's title attribute.
export function detailLabel(s: string): string {
  const collapsed = s.replace(/\s+/g, " ").trim();
  return collapsed.length <= 80 ? collapsed : `${collapsed.slice(0, 80)}…`;
}

// web.BaseName: the last path segment of a file path, handling both separators.
export function baseName(p: string): string {
  const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return i >= 0 && i < p.length - 1 ? p.slice(i + 1) : p;
}

// web.ToolFilePath / web.ToolFileTitle: the display path prefers the
// worktree-relative form; the absolute path rides the hover title only when it
// differs from what is shown.
export function toolFilePath(t: {
  FileRelPath: string;
  FilePath: string;
}): string {
  return t.FileRelPath || t.FilePath;
}

export function toolFileTitle(t: {
  FileRelPath: string;
  FilePath: string;
}): string {
  return t.FileRelPath && t.FilePath && t.FileRelPath !== t.FilePath
    ? t.FilePath
    : "";
}

// web.StripPromptPreamble: reduces a session's first-message title to the part a
// reader cares about, stripping harness wrapper blocks and slash-command envelopes.
const harnessWrapperTags = [
  "local-command-caveat",
  "local-command-stdout",
  "command-message",
  "command-name",
  "command-args",
  "system-reminder",
];

function tagContent(s: string, tag: string): string {
  const open = `<${tag}>`;
  const close = `</${tag}>`;
  const i = s.indexOf(open);
  if (i < 0) return "";
  const rest = s.slice(i + open.length);
  const j = rest.indexOf(close);
  return j < 0 ? "" : rest.slice(0, j);
}

function slashCommandName(t: string): string {
  const name = tagContent(t, "command-name").trim();
  if (!name) return "";
  const args = tagContent(t, "command-args").trim();
  return args ? `${name} ${args}` : name;
}

function stripLeadingTagBlocks(input: string): string {
  let t = input.trim();
  for (;;) {
    let advanced = false;
    for (const tag of harnessWrapperTags) {
      const open = `<${tag}>`;
      const close = `</${tag}>`;
      if (!t.startsWith(open)) continue;
      const k = t.indexOf(close);
      if (k >= 0) {
        t = t.slice(k + close.length).trim();
        advanced = true;
        break;
      }
    }
    if (!advanced) return t;
  }
}

export function stripPromptPreamble(title: string): string {
  const trimmed = title.trim();
  if (!trimmed) return title;
  const command = slashCommandName(trimmed);
  if (command) return command;
  let t = stripLeadingTagBlocks(trimmed);
  if (t.startsWith("# AGENTS.md instructions")) {
    const i = t.indexOf("</INSTRUCTIONS>");
    if (i >= 0) {
      const rest = t.slice(i + "</INSTRUCTIONS>".length).trim();
      if (rest) t = rest;
    }
  }
  t = t.trim();
  return t || title;
}
