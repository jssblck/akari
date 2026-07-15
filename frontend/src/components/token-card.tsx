import {
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";

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
//
// The card is a native top-layer popover, so it always paints above the page
// and no ancestor's overflow, stacking context, or z-index can trap it. Top
// layer means viewport coordinates: the card is placed centered above the
// trigger, clamped to the viewport, and flipped below when the trigger sits
// too close to the top.
export function HoverTip({
  summary,
  children,
  className = "",
}: {
  summary: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const wrapRef = useRef<HTMLSpanElement>(null);
  const tipRef = useRef<HTMLSpanElement>(null);
  const [open, setOpen] = useState(false);

  const show = () => {
    const wrap = wrapRef.current;
    const tip = tipRef.current;
    if (!wrap || !tip || typeof tip.showPopover !== "function") return;
    try {
      tip.showPopover();
    } catch {
      // Already open (a focus following a hover); reposition below.
    }
    const margin = 8;
    const anchor = wrap.getBoundingClientRect();
    const card = tip.getBoundingClientRect();
    const left = Math.max(
      margin,
      Math.min(
        anchor.left + anchor.width / 2 - card.width / 2,
        window.innerWidth - card.width - margin,
      ),
    );
    let top = anchor.top - card.height - margin;
    if (top < margin) top = anchor.bottom + margin;
    tip.style.left = `${Math.round(left)}px`;
    tip.style.top = `${Math.round(top)}px`;
    setOpen(true);
  };
  const hide = useCallback(() => {
    try {
      tipRef.current?.hidePopover();
    } catch {
      // Already closed.
    }
    setOpen(false);
  }, []);

  // A top-layer card holds viewport coordinates, so scrolling would leave it
  // drifting away from its trigger; close it instead.
  useEffect(() => {
    if (!open) return;
    window.addEventListener("scroll", hide, true);
    return () => window.removeEventListener("scroll", hide, true);
  }, [open, hide]);

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: the WAI tooltip pattern hangs hover and focus reveal off a non-interactive trigger; the handlers only toggle the card.
    <span
      className={`hover-tip ${className}`.trim()}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: the WAI tooltip pattern needs a focusable trigger so the card is reachable by keyboard, and the trigger itself performs no action.
      tabIndex={0}
      ref={wrapRef}
      onMouseEnter={show}
      onMouseLeave={hide}
      onFocus={show}
      onBlur={hide}
      // A manual popover opts out of the UA's Escape handling, so dismiss-
      // without-blurring is wired up here per the WAI tooltip pattern.
      onKeyDown={(event) => {
        if (event.key === "Escape") hide();
      }}
    >
      {summary}
      <span
        className="tip-card popover"
        role="tooltip"
        popover="manual"
        ref={tipRef}
      >
        {children}
      </span>
    </span>
  );
}
