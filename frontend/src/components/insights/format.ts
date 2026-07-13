// Formatters specific to the Insights chart engine. These mirror the old
// insights.js formatters (fmtInt/fmtK/fmtPct/fmtS) and the Go web package's
// insights_data.go helpers (fmtCostShort, fmtDurationShort, FmtSnapshotAge,
// prettyModel) exactly, since captions and tooltips must read identically to
// the port's source of truth. frontend/src/format.ts already covers the
// app-wide cost/token/count formatters used elsewhere; these are the ones
// unique to this page's charts.

export function fmtInt(n: number): string {
  return Math.round(n).toLocaleString("en-US");
}

// fmtK mirrors the server's compact-magnitude formatting used on chart axes
// (1.2k / 12k), distinct from formatTokens (which adds M/B bands the chart
// axes here never reach).
export function fmtK(n: number): string {
  if (n >= 1000) {
    const digits = n >= 10000 ? 0 : 1;
    return `${(n / 1000).toFixed(digits).replace(/\.0$/, "")}k`;
  }
  return String(Math.round(n));
}

export function fmtPct(n: number, decimals = 0): string {
  return `${n.toFixed(decimals)}%`;
}

export function fmtS(n: number): string {
  return `${Math.round(n)}s`;
}

// fmtDuration renders a chart-axis duration label (30s / 5m / 1.0h), the x
// gridline labels on the session gallery's log duration axis.
export function fmtDuration(s: number): string {
  if (s < 90) return `${Math.round(s)}s`;
  if (s < 3600) return `${(s / 60).toFixed(0)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

// fmtDurationShort is the gallery's outlier-callout label: a shorter, coarser
// form than fmtDuration (whole minutes/seconds, one decimal hour).
export function fmtDurationShort(secs: number): string {
  if (secs >= 3600) return `${(secs / 3600).toFixed(1)}h`;
  if (secs >= 60) return `${(secs / 60).toFixed(0)}m`;
  return `${Math.round(secs)}s`;
}

// fmtCostShort is the gallery's priciest-session callout label: whole dollars
// past $100, cents below it, no lower-bound marker (the callout points at a
// specific point already drawn with its own cost).
export function fmtCostShort(usd: number): string {
  if (usd >= 100) return `$${usd.toFixed(0)}`;
  return `$${usd.toFixed(2)}`;
}

// prettyModel shortens a model identifier for a legend chip, matching the
// server's insights_data.go prettyModel exactly.
export function prettyModel(m: string): string {
  if (m === "" || m === "unknown") return "unknown";
  let s = m;
  if (s.startsWith("claude-")) s = s.slice("claude-".length);
  if (s.startsWith("anthropic/")) s = s.slice("anthropic/".length);
  return s;
}

export function titleCase(s: string): string {
  if (!s) return s;
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// snapshotAge mirrors internal/server/web/freshness.go's FmtSnapshotAge
// verbatim: the Insights page is served from an hourly precomputed snapshot,
// not live data, so the header note names the snapshot's age rather than
// implying the figures are current.
export function snapshotAge(now: Date, at: Date): string {
  const deltaMs = now.getTime() - at.getTime();
  const minutes = deltaMs / 60000;
  const hours = deltaMs / 3600000;
  if (deltaMs < 90_000) return "updated just now";
  if (deltaMs < 3_600_000) return `updated ${Math.floor(minutes)} min ago`;
  if (deltaMs < 48 * 3_600_000) return `updated ${Math.floor(hours)} hr ago`;
  return `updated ${Math.floor(hours / 24)} days ago`;
}

// vizVars is the ordered ramp assigned to ranked series (models, projects),
// matching the server's vizVars: 8 slots, ordinal, "other" never consumes one.
export const vizVars = [
  "var(--viz-1)",
  "var(--viz-2)",
  "var(--viz-3)",
  "var(--viz-4)",
  "var(--viz-5)",
  "var(--viz-6)",
  "var(--viz-7)",
  "var(--viz-8)",
];

// vizRgb is vizVars' resolved RGB, for the treemap's cell-shading math (CSS
// custom properties cannot be mixed at paint time the way an inline rgb()
// composite can). Values match styles.css's --viz-1..8 exactly.
export const vizRgb: Record<string, [number, number, number]> = {
  "var(--viz-1)": [198, 168, 242],
  "var(--viz-2)": [136, 207, 206],
  "var(--viz-3)": [240, 191, 146],
  "var(--viz-4)": [236, 152, 176],
  "var(--viz-5)": [166, 210, 158],
  "var(--viz-6)": [149, 192, 239],
  "var(--viz-7)": [221, 200, 133],
  "var(--viz-8)": [169, 138, 212],
};

// pickVizVar wraps any index into the 8-slot ramp, so an ordinal color
// assignment never has to special-case running out of slots (it simply
// repeats the ramp) and never has to prove to the type checker that a
// modulo result stays in bounds.
export function pickVizVar(i: number): string {
  const idx = ((i % vizVars.length) + vizVars.length) % vizVars.length;
  return vizVars[idx] ?? "var(--viz-1)";
}

export const CATEGORY_COLOR: Record<string, string> = {
  bash: "var(--viz-1)",
  edit: "var(--viz-4)",
  read: "var(--viz-2)",
  search: "var(--viz-6)",
  write: "var(--viz-3)",
  other: "var(--viz-8)",
};

export const CATEGORY_LABEL: Record<string, string> = {
  bash: "Shell",
  edit: "Edit",
  read: "Read",
  search: "Search",
  write: "Write",
  other: "Other",
};

export function categoryColor(cat: string): string {
  return CATEGORY_COLOR[cat] ?? "var(--viz-8)";
}

export function categoryLabel(cat: string): string {
  return CATEGORY_LABEL[cat] ?? titleCase(cat);
}

export const ARCHETYPE_LABEL: Record<string, string> = {
  quick: "Quick",
  standard: "Standard",
  deep: "Deep",
  marathon: "Marathon",
  automation: "Automation",
};

// archetypeColor matches the gallery scatter's swatches (insights_data.go's
// archetypeColor), distinct from the health instrument's archetype-share
// stack colors (which follow the plain vizVars ramp instead).
export const ARCHETYPE_COLOR: Record<string, string> = {
  quick: "var(--viz-2)",
  standard: "var(--viz-4)",
  deep: "var(--viz-6)",
  marathon: "var(--viz-7)",
  automation: "var(--viz-8)",
};

export const ARCHETYPE_ORDER = [
  "quick",
  "standard",
  "deep",
  "marathon",
  "automation",
];

export function archetypeColor(key: string): string {
  return ARCHETYPE_COLOR[key] ?? "var(--viz-8)";
}

export function archetypeLabel(key: string): string {
  return ARCHETYPE_LABEL[key] ?? titleCase(key);
}

export const GRADE_ORDER = ["A", "B", "C", "D", "F", "U"];
export const GRADE_COLOR: Record<string, string> = {
  A: "var(--viz-5)",
  B: "var(--viz-2)",
  C: "var(--viz-7)",
  D: "var(--viz-3)",
  F: "var(--viz-4)",
  U: "var(--faint)",
};
export const GRADE_LABEL: Record<string, string> = {
  A: "A",
  B: "B",
  C: "C",
  D: "D",
  F: "F",
  U: "unscored",
};

export function gradeColor(key: string): string {
  return GRADE_COLOR[key] ?? "var(--faint)";
}

export function gradeLabel(key: string): string {
  return GRADE_LABEL[key] ?? key;
}

export const FANOUT_COLOR: Record<string, string> = {
  one: "rgba(198,168,242,0.30)",
  twoThree: "rgba(198,168,242,0.52)",
  fourSeven: "rgba(198,168,242,0.74)",
  eightPlus: "rgba(198,168,242,0.96)",
};

export const FANOUT_LABEL: Record<string, string> = {
  one: "1",
  twoThree: "2-3",
  fourSeven: "4-7",
  eightPlus: "8+",
};

export function fanoutColor(key: string): string {
  return FANOUT_COLOR[key] ?? "var(--viz-1)";
}

export function fanoutLabel(key: string): string {
  return FANOUT_LABEL[key] ?? key;
}

// nsToSeconds converts an Insights.Velocity Duration field (JSON-serialized
// nanoseconds) to seconds.
export function nsToSeconds(ns: number): number {
  return ns / 1e9;
}
