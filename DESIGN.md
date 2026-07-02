---
name: akari
description: A self-hosted instrument for reading AI coding-agent sessions.
colors:
  bg: "#14131b"
  surface: "#1b1a24"
  surface-2: "#232230"
  surface-3: "#2b2939"
  border: "#383548"
  border-strong: "#4a4660"
  text: "#e6e3f0"
  subtext: "#c5c1d6"
  muted: "#9a94ad"
  faint: "#6f6987"
  lilac: "#c6a8f2"
  lilac-strong: "#b693ea"
  ink-on-lilac: "#1a1320"
  ok: "#9fd6a6"
  err: "#ef9aa9"
  warn: "#f0c592"
  info: "#92cfd4"
  msg-user: "#272336"
  msg-assistant: "#1f1f2b"
  viz-1-lilac: "#c6a8f2"
  viz-2-teal: "#88cfce"
  viz-3-peach: "#f0bf92"
  viz-4-rose: "#ec98b0"
  viz-5-sage: "#a6d29e"
  viz-6-sky: "#95c0ef"
  viz-7-gold: "#ddc885"
  viz-8-mauve: "#a98ad4"
typography:
  display:
    fontFamily: "Geist, Inter, system-ui, -apple-system, sans-serif"
    fontSize: "1.75rem"
    fontWeight: 600
    lineHeight: 1.15
    letterSpacing: "-0.02em"
  headline:
    fontFamily: "Geist, Inter, system-ui, sans-serif"
    fontSize: "1.25rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.01em"
  title:
    fontFamily: "Geist, Inter, system-ui, sans-serif"
    fontSize: "1rem"
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "normal"
  body:
    fontFamily: "Geist, Inter, system-ui, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.55
    letterSpacing: "normal"
  label:
    fontFamily: "Geist, Inter, system-ui, sans-serif"
    fontSize: "0.6875rem"
    fontWeight: 500
    lineHeight: 1.2
    letterSpacing: "0.08em"
  data:
    fontFamily: "Geist Mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "0.8125rem"
    fontWeight: 450
    lineHeight: 1.4
    letterSpacing: "normal"
  code:
    fontFamily: "Geist Mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "0.75rem"
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: "normal"
rounded:
  sm: "4px"
  md: "6px"
  lg: "10px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "24px"
  xxl: "32px"
components:
  button-primary:
    backgroundColor: "{colors.lilac}"
    textColor: "{colors.ink-on-lilac}"
    typography: "{typography.body}"
    rounded: "{rounded.sm}"
    padding: "8px 14px"
  button-primary-hover:
    backgroundColor: "{colors.lilac-strong}"
    textColor: "{colors.ink-on-lilac}"
  button-secondary:
    backgroundColor: "{colors.surface-2}"
    textColor: "{colors.text}"
    rounded: "{rounded.sm}"
    padding: "8px 14px"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.subtext}"
    rounded: "{rounded.sm}"
    padding: "8px 14px"
  button-danger:
    backgroundColor: "{colors.surface-2}"
    textColor: "{colors.err}"
    rounded: "{rounded.sm}"
    padding: "8px 14px"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.sm}"
    padding: "7px 10px"
  tag:
    backgroundColor: "{colors.surface-2}"
    textColor: "{colors.muted}"
    typography: "{typography.label}"
    rounded: "{rounded.sm}"
    padding: "2px 7px"
  tag-agent:
    backgroundColor: "{colors.surface-2}"
    textColor: "{colors.lilac}"
    rounded: "{rounded.sm}"
    padding: "2px 7px"
  card:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "16px"
  stat-tile:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "10px 14px"
  tool-chip:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.subtext}"
    typography: "{typography.data}"
    rounded: "{rounded.sm}"
    padding: "5px 10px"
  msg-user:
    backgroundColor: "{colors.msg-user}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "12px 14px"
  msg-assistant:
    backgroundColor: "{colors.msg-assistant}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "12px 14px"
---

# Design System: akari

## 1. Overview

**Creative North Star: "The Machinist's Bench"**

akari is an instrument, not a dashboard. The model is a machinist's bench under
low shop light: steel surfaces with a faint violet cast, scribed hairlines
instead of heavy frames, knurled-cap labels over precise readouts, and every
number machined to tolerance in a monospace face. The reader is an engineer who
wants a measured tool, so the interface measures and reports.

The palette is the surprise. The bench is rendered in muted lilac and pastel
signal colors on a deep violet-graphite base (a Catppuccin-Mocha sensibility,
not the literal scheme). Desaturation is the discipline, so pastels stay quiet on
a genuinely dark ground and the surface reads as serious tooling rather than a
toy. Color is a signal, never decoration. The lilac accent appears on a small
fraction of any screen and means "act here" or "live now"; the rest is steel and
ink.

This system explicitly rejects four things, carried verbatim from the product's
anti-references. It is **not generic dark SaaS** (no interchangeable
one-blue-accent admin look; the signature is lilac, and blue is demoted to a
single chart series). It is **not enterprise bloat** (authorization is flat, so
the chrome stays light: no settings-soup, no modal-on-modal). It is **not
playful or consumer** (no candy colors, no soft mascot roundness; pastels are
muted and edges are tight). And it avoids **Datadog-style clutter** (real charts
and dense data are welcome, the everything-at-once wall of panels is not).

**Key Characteristics:**
- Deep violet-graphite base; muted lilac signature carried on under 10% of a screen.
- Scribed 1px hairlines and tonal layering do the structural work; shadows are rare.
- Every figure is monospaced and tabular, so values never shift as data streams.
- Tight radii (4 to 10px) and machined rectangular tags, never consumer pills.
- Motion settles like an instrument needle: fast and reversible.

## 2. Colors

A deep violet-graphite ground under a muted lilac signal and a set of desaturated
pastel status and data hues. Every text pairing below is verified at WCAG AA or
better against its intended background.

### Primary
- **Machined Lilac** (`#c6a8f2`): the one signature. Primary actions, links,
  focus rings, the active filter, the live-update glow, and the primary chart
  series. Carried on a small fraction of any screen; its rarity is what makes it
  read as signal. On a lilac fill, text is **Bench Ink** (`#1a1320`).
- **Lilac Deep** (`#b693ea`): hover and active state for lilac surfaces only.

### Secondary (status signals)
- **Sage** (`#9fd6a6`): success, a healthy or complete state.
- **Rose** (`#ef9aa9`): errors and destructive intent.
- **Peach** (`#f0c592`): in-progress, incomplete, or estimated values (for
  example a partial or incomplete cost).
- **Teal** (`#92cfd4`): neutral informational emphasis.

### Tertiary (data visualization)
An eight-step categorical ramp for charts, ordered for maximum separation on the
dark ground: **Lilac** `#c6a8f2`, **Teal** `#88cfce`, **Peach** `#f0bf92`,
**Rose** `#ec98b0`, **Sage** `#a6d29e`, **Sky** `#95c0ef`, **Gold** `#ddc885`,
**Mauve** `#a98ad4`. Sky blue lives here and only here: blue is a data series,
never the brand. For intensity ramps (a heatmap, a usage density), step the
lightness of Machined Lilac rather than mixing hues.

### Neutral
- **The Room** (`#14131b`): the page ground, a deep violet-tinted near-black.
- **Surface** (`#1b1a24`) / **Surface Raised** (`#232230`) / **Surface Elevated**
  (`#2b2939`): the tonal ladder. Depth is built by stepping these, not by shadow.
- **Scribe Line** (`#383548`) and **Scribe Strong** (`#4a4660`): 1px hairline
  borders. The default frame is one scribed line, not a heavy box.
- **Text** (`#e6e3f0`): primary reading color. **Subtext** (`#c5c1d6`): secondary.
  **Muted** (`#9a94ad`): labels, captions, and placeholders (held to full body
  contrast, never washed out). **Faint** (`#6f6987`): non-text dividers only, never
  type.
- **Message surfaces**: user turns sit on warm violet **`#272336`**, assistant
  turns on cooler steel **`#1f1f2b`**, so author is legible by tone alone.

### Named Rules
**The One Voice Rule.** Machined Lilac covers at most 10% of any screen. If two
things on screen are lilac, one of them is wrong. Steel and ink carry the rest.

**The Blue Demotion Rule.** Blue is forbidden as an interface accent (it is the
generic-SaaS tell). It survives only as Sky, one series in the chart ramp.

**The Muted Pastel Rule.** Every signal color is desaturated against a genuinely
dark ground. If a hue looks candy-bright, lower its chroma until it reads as an
instrument light, not a sticker.

## 3. Typography

**Display / UI Font:** Geist (with Inter, then system-ui as fallback)
**Data / Mono Font:** Geist Mono (with ui-monospace, SFMono-Regular, Menlo)

**Character:** one clean engineered superfamily across two axes. Geist is a
precise, low-character grotesque that keeps the chrome quiet; Geist Mono machines
every number, id, duration, model name, and code body to a fixed grid. The
contrast is sans-for-prose against mono-for-measurement, not two competing
display faces. Self-host both as woff2 in the embedded static assets; the binary
stays self-contained with no Node toolchain.

### Hierarchy
- **Display** (Geist 600, 1.75rem, line-height 1.15, -0.02em): page titles only,
  for example a project name or "Search". The scale ceiling stays modest on purpose.
- **Headline** (Geist 600, 1.25rem, -0.01em): section headers such as
  "Transcript" and "Subagents".
- **Title** (Geist 600, 1rem): card and panel titles.
- **Body** (Geist 400, 0.875rem, line-height 1.55): default UI and message text.
  Cap reading measure at 75ch in the transcript.
- **Label** (Geist 500, 0.6875rem, 0.08em, uppercase): the knurled caps over
  table headers and stat-tile labels. This is the one place uppercase tracking is
  sanctioned; it is the machined cap stamped on a readout.
- **Data** (Geist Mono 450, 0.8125rem, tabular-nums): every metric, token count,
  cost, id, branch, and duration. Tabular figures are mandatory so columns and
  live values never shift.
- **Code** (Geist Mono 400, 0.75rem, line-height 1.5): expanded tool input and
  result bodies.

### Named Rules
**The Tabular Tolerance Rule.** Every number renders in Geist Mono with
`font-variant-numeric: tabular-nums`. A value that jitters by a pixel as it
updates is a defect, like a gauge with a loose needle.

**The Caps-Once Rule.** Uppercase tracking is permitted only on the 11px label
role. Never set body text, buttons, or headings in tracked uppercase; that is a
2014 SaaS reflex.

## 4. Elevation

Flat by doctrine. Depth comes from the tonal surface ladder (The Room to Surface
to Raised to Elevated) and from scribed 1px hairlines, not from drop shadows.
This is the bench, machined plates layered by tone and separated by scribed lines.
Shadow is reserved for things that genuinely float above the page.

### Shadow Vocabulary
- **Overlay** (`box-shadow: 0 8px 24px -6px rgba(8, 6, 14, 0.6)`): dropdowns,
  the native dialog/popover, and any menu that escapes the layout. The only
  ambient shadow in the system.
- **Focus Ring** (`box-shadow: 0 0 0 2px rgba(198, 168, 242, 0.5)`): a 2px lilac
  ring on keyboard focus and active inputs. Focus is a lilac glow, not a browser
  outline.

### Named Rules
**The Scribed Line Rule.** Surfaces are bordered by a single 1px hairline in
Scribe Line, not by a heavy frame or a dark inset. A component that needs a
2px-plus colored edge to separate is layered wrong; restate it with tone.

**The Float-Earns-Shadow Rule.** A shadow is allowed only on something that
actually overlaps other content (a menu, a dialog). Resting cards, tiles, and
rows are flat. A shadow on a static card is a 2014 tell.

## 5. Components

### Buttons
- **Shape:** tight rectangles, 4px radius (`{rounded.sm}`), 8px by 14px padding.
- **Primary:** Machined Lilac fill with Bench Ink text. The only filled-lilac
  element in a view; reserve it for the one primary action.
- **Secondary:** Surface Raised fill, Text color, 1px Scribe Line border.
- **Ghost:** transparent, Subtext color; the low-emphasis action.
- **Danger:** Surface Raised fill with Rose text and a Rose hairline; on hover it
  inverts to a Rose fill with Bench Ink. Destructive intent is never a filled
  red button at rest.
- **Hover / Focus:** background shifts to Lilac Deep (primary) or Surface
  Elevated (secondary) over 120ms; keyboard focus adds the 2px lilac ring.

### Tags (badges)
- **Style:** machined rectangles at 4px radius, Surface Raised fill, 11px Label
  type. Agent tags carry Lilac text; a public tag carries Sage; a status tag
  takes its signal hue.
- **The Tag Is Not A Pill.** Never render a fully rounded pill badge; that is the
  consumer tell. Tags are small stamped rectangles.

### Stat Tiles (instrument readouts)
- **Style:** Surface card, 6px radius, a Muted 11px uppercase Label over a large
  Geist Mono value. These are the gauges: Messages, Input, Output, Cache, Cost,
  Duration.
- **Live behavior:** when a value updates over SSE, the value briefly underglows
  Lilac and settles (see Motion). Reduced motion swaps the value instantly.

### Inputs / Fields
- **Style:** Surface fill, 1px Scribe Line border, 4px radius, Text color, Geist
  body.
- **Focus:** border shifts to Lilac and the 2px lilac ring appears. Placeholders
  use Muted (full contrast), never Faint.
- **Error:** border and helper text shift to Rose; the message names the problem,
  it does not just color the field.

### Tables (the data grid)
The primary surface for the projects index, session lists, and account tables.
- Hairline row separators in Scribe Line; no vertical rules, no zebra fill.
- Header cells use the 11px uppercase Muted Label.
- Numeric columns are right-aligned Geist Mono with tabular figures.
- Row hover tints to Surface Raised over 120ms; the whole row is the hit target.

### Cards / Containers
- **Corner:** 6px radius (`{rounded.md}`).
- **Background:** Surface, against the Room ground.
- **Border:** a single 1px Scribe Line. Flat; no resting shadow.
- **Padding:** 16px (`{spacing.lg}`). Never nest a card inside a card.

### Navigation
- A sticky top bar on Surface with a 1px Scribe Line underneath. Brand wordmark at
  left in Geist 600, primary links in Body, the username in Muted at right.
- Active and hover links take Lilac; the rest are Subtext. No heavy active pill.

### Tool Chip (signature component)
The defining element of the transcript: a tool call rendered as a compact metadata
chip. It shows the tool name in Lilac, an optional file path in Muted, and the
input/output bodies as size-and-type stamps (for example "in: 36 KB json"). A
stamp backed by a stored body is a button; clicking expands the body inline as a
Geist Mono code block fetched from the content store, with a 180ms height-and-fade
reveal. The result status reads in its signal hue (Sage ok, Rose error). A tool
with no file path (a shell command, a search pattern, a fetched URL) instead shows
a bounded, one-line summary of its input in Muted, truncated with an ellipsis and
carrying the full text on hover. The chip is the bench's calipers: small and
exact, and it opens to show the measurement.

### Charts (signature direction)
akari has no charts yet; this is the brief for adding them. Render time series
(cost and tokens over time, per project and across the fleet) and breakdowns
(model mix, agent mix) as first-class instruments.
- **Engine:** a lightweight canvas time-series library loaded as a single static
  asset (uPlot is the reference: tiny, fast, no build step, matches the templ and
  htmx, no-Node posture). Small inline sparklines and bars in table cells may be
  server-rendered SVG.
- **Ground and grid:** transparent on the Room; gridlines in Scribe Line at low
  alpha; axis ticks and labels in Geist Mono.
- **Series:** the eight-step data ramp in order, Machined Lilac first. A single
  metric uses Lilac alone with a soft lilac area fill below the line.
- **Interaction:** a thin lilac crosshair on hover with a mono readout of the
  value under the cursor; the measurement-first nod to the instrument.

## 6. Do's and Don'ts

### Do:
- **Do** carry Machined Lilac (`#c6a8f2`) on at most 10% of a screen, reserved for
  the primary action, links, focus, and the live signal. Steel and ink do the rest.
- **Do** set every number in Geist Mono with `tabular-nums`, so values hold their
  position as data streams in.
- **Do** build depth from the tonal surface ladder and 1px scribed hairlines;
  keep resting surfaces flat.
- **Do** keep radii tight (4 to 10px) and render badges as machined rectangles.
- **Do** desaturate every pastel against the dark ground until it reads as an
  instrument light, and back every status hue with a label or icon, never color
  alone.
- **Do** give live updates a 120 to 220ms settle (the needle easing) and a
  `prefers-reduced-motion` instant fallback.

### Don't:
- **Don't** ship **generic dark SaaS**: no interchangeable one-blue-accent admin
  look. Blue is demoted to a single chart series (Sky); the signature is lilac.
- **Don't** add **enterprise bloat**: no settings-soup, no modal-on-modal. The
  authorization model is flat, so the chrome stays light.
- **Don't** drift **playful or consumer**: no candy-bright colors, no fully
  rounded pills, no soft mascot roundness. Muted pastels and tight edges only.
- **Don't** reproduce **Datadog-style clutter**: real charts and dense tables are
  welcome, the everything-at-once wall of panels is not. Density is earned per
  element.
- **Don't** use a `border-left` or `border-right` thicker than 1px as a colored
  accent stripe on cards, rows, or callouts. Use a full hairline, a tonal fill, or
  a leading tag instead.
- **Don't** set body text, buttons, or headings in tracked uppercase; reserve caps
  for the 11px label role only.
- **Don't** put a drop shadow on a resting card, tile, or row. Shadow is for
  floating overlays alone.
