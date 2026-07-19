# Product

## Register

product

## Users

Developers and small engineering teams who run AI coding agents (Claude Code,
Codex, pi) across several machines and want one shared, searchable history of
everything those agents did. They self-host akari; logged in means they see
every session, so there is no per-user gatekeeping to design around.

Their context when using it:

- **Debugging a run.** Something an agent did needs explaining. They open a
  single session and read the transcript: messages, thinking, tool calls and
  their bodies, timing, attachments, any subagents it spawned.
- **Watching the spend.** They want to know where tokens and money are going,
  per project and over time, and which models and agents drive it.
- **Finding a past session.** Trigram search across message content, scoped to a
  project or global, to recover the run they half-remember.
- **Sharing one out.** Occasionally they publish a single session for a
  logged-out viewer over an unguessable link.

They are technical, comfortable with dense information, almost always on desktop,
and often reading for a long stretch.

## Product Purpose

akari is an explicit client/server split: many thin clients push raw session
bytes to one Linux server that parses, prices, stores, and renders them. The
client keeps no derived state, so a parser improvement reaches old sessions by
re-parsing on the server with nothing re-uploaded.

It exists so a team's agent work is backed up, comparable, and legible in one
place, keyed by canonical git remote so the same repo across worktrees and
machines collapses into one project. In-progress sessions stream in live over
server-sent events.

Success means a developer can land on akari and within seconds either read
exactly what an agent did in a run, or see where tokens and cost are going across
projects, without fighting the interface. The data is the product, and the UI
stays out of its way.

## Brand Personality

Precise, calm, fast. An instrument you read, not a dashboard you configure.

- **Voice:** terse and exact. Numbers you believe, presented as tabular and
  stable figures that do not jitter as data streams in. The interface reports; it
  does not editorialize.
- **Material:** dark and industrial. Control-room surfaces, a structural grid,
  monospaced numerics, restrained metal-on-dark contrast. The aesthetic is
  serious tooling.
- **Composition:** calm and legible. Density is welcome but never clutter;
  hierarchy and spacing carry the load so a busy screen still reads quietly.
- **Motion:** responsive, not decorative. Movement communicates state (a live
  message arriving, a tool body expanding), and the tool should feel instant.

Emotional goal: confidence and flow. The tool should feel exact and unobtrusive,
never overwhelming.

## Anti-references

- **Generic dark SaaS.** The interchangeable one-blue-accent dashboard look that
  reads as a template. akari should not be mistaken for every other dark admin
  panel.
- **Enterprise bloat.** Settings-soup, modal-on-modal, slow corporate heaviness.
  Authorization here is deliberately flat; the UI should feel that lightness.
- **Playful or consumer.** Rounded mascot friendliness, candy colors, soft
  edges. This is an instrument, not a toy.
- **Datadog-style clutter.** Keep the good of mature observability tools (legible
  dense data, real charts) without the everything-at-once wall of panels. Density
  is earned per element, not sprayed across the screen.

## Design Principles

1. **Instrument, not dashboard.** Figures use tabular numerics and do not reflow
   as data streams. Dollar values are application-wide best-effort estimates.
   The UI measures and reports rather than persuades.
2. **Density without clutter.** High information density is a feature for this
   reader, but every element earns its place. Whitespace and hierarchy do the
   calming, so a dense screen still reads quietly.
3. **Fast is a feature.** The tool should feel instant. Motion exists to
   communicate state (live arrivals, expansions, filter changes), never to
   decorate, and latency or layout shift is treated as a bug.
4. **Two front doors, equal craft.** The single-session trace view and the
   cross-fleet analytics are co-heroes. Neither the deep read nor the wide
   overview is a second-class surface.
5. **Legible under load.** Built for long reading and large transcripts:
   comfortable at small sizes, calm contrast, status carried by more than color,
   nothing that strains the eye over an hour.

## Accessibility & Inclusion

- **WCAG 2.1 AA floor.** Body text meets at least 4.5:1 against its background;
  large text and UI affordances meet at least 3:1. Placeholder and muted text are
  held to the same body contrast, not the washed-out gray default.
- **Status never by color alone.** ok, error, and agent states carry an icon,
  label, or shape in addition to hue, so red/green and the blue accent stay
  legible under color-vision deficiency.
- **Reduced motion is supported, not optional.** Every transition has a crossfade
  or instant fallback under `prefers-reduced-motion: reduce`. Live updates and
  expansions still function with motion suppressed.
- **Stable numerics.** All metrics use tabular figures so values do not shift
  position as they update.
- **Keyboard and focus.** Interactive controls are reachable by keyboard with a
  visible focus state. Desktop is the primary context, but the layout must not
  break at smaller widths.
