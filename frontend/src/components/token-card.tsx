import type { ReactNode } from "react";

import { formatCost, formatTokens } from "../format";

// TokenCard is the shared per-class token breakdown (In / Out / Cache read /
// Cache write, optional Reasoning line, optional cost footer) that surfaces on
// hover or keyboard focus wherever a summed token figure appears: stat tiles,
// breakdown rows, session feed rows. One card so a token total reads the same
// everywhere.
export function TokenCard({
  input,
  output,
  cacheRead,
  cacheWrite,
  reasoning = 0,
  costUSD,
  costIncomplete = false,
}: {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  reasoning?: number;
  costUSD?: number;
  costIncomplete?: boolean;
}) {
  return (
    <>
      <dl className="tt-grid">
        <dt>In</dt>
        <dd>{formatTokens(input)}</dd>
        <dt>Out</dt>
        <dd>{formatTokens(output)}</dd>
        <dt>Cache read</dt>
        <dd>{formatTokens(cacheRead)}</dd>
        <dt>Cache write</dt>
        <dd>{formatTokens(cacheWrite)}</dd>
        {reasoning > 0 ? (
          <>
            <dt>Reasoning</dt>
            <dd>{formatTokens(reasoning)}</dd>
          </>
        ) : null}
      </dl>
      {costUSD !== undefined ? (
        <div className="tt-cost">{formatCost(costUSD, costIncomplete)}</div>
      ) : null}
    </>
  );
}

// HoverTip wraps an inline figure with a card revealed on hover or keyboard
// focus. The wrapper is focusable so the breakdown is reachable without a
// pointer, matching the old UI's .tok-cell / .stat-tip convention.
export function HoverTip({
  summary,
  children,
  className = "",
}: {
  summary: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: the WAI tooltip pattern needs a focusable trigger so the card is reachable by keyboard, and the trigger itself performs no action.
    <span className={`hover-tip ${className}`.trim()} tabIndex={0}>
      {summary}
      <span className="tip-card popover" role="tooltip">
        {children}
      </span>
    </span>
  );
}
