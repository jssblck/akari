import {
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";

import { formatCost, formatTokens } from "../format";

type PointerPosition = { clientX: number; clientY: number };

// useHoverPopover keeps every tooltip on the same positioning model. Pointer
// opens capture their initial horizontal cursor position and stay there, while
// the vertical gap is measured from the trigger's edge. Keyboard opens use the
// trigger center. Both prefer above and flip below at the viewport edge.
export function useHoverPopover<
  Trigger extends HTMLElement,
  Card extends HTMLElement,
>() {
  const triggerRef = useRef<Trigger>(null);
  const popoverRef = useRef<Card>(null);
  const openRef = useRef(false);
  const [open, setOpen] = useState(false);

  const show = useCallback((pointer?: PointerPosition) => {
    const trigger = triggerRef.current;
    const card = popoverRef.current;
    if (
      openRef.current ||
      !trigger ||
      !card ||
      typeof card.showPopover !== "function"
    )
      return;
    try {
      card.showPopover();
    } catch {
      // The native popover may already be open after a browser-managed focus.
    }
    const margin = 8;
    const triggerRect = trigger.getBoundingClientRect();
    const cardRect = card.getBoundingClientRect();
    const pointerOpen =
      pointer &&
      Number.isFinite(pointer.clientX) &&
      Number.isFinite(pointer.clientY);
    const anchorX = pointerOpen
      ? pointer.clientX
      : triggerRect.left + triggerRect.width / 2;
    const left = Math.max(
      margin,
      Math.min(
        anchorX - cardRect.width / 2,
        window.innerWidth - cardRect.width - margin,
      ),
    );
    let top = triggerRect.top - cardRect.height - margin;
    if (top < margin) top = triggerRect.bottom + margin;
    card.style.left = `${Math.round(left)}px`;
    card.style.top = `${Math.round(top)}px`;
    openRef.current = true;
    setOpen(true);
  }, []);

  const hide = useCallback(() => {
    try {
      popoverRef.current?.hidePopover();
    } catch {
      // Already closed.
    }
    openRef.current = false;
    setOpen(false);
  }, []);

  // A top-layer card holds viewport coordinates, so scrolling would leave it
  // drifting away from its trigger; close it instead.
  useEffect(() => {
    if (!open) return;
    window.addEventListener("scroll", hide, true);
    return () => window.removeEventListener("scroll", hide, true);
  }, [open, hide]);

  return { triggerRef, popoverRef, show, hide };
}

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
// layer means viewport coordinates: pointer opens align horizontally with the
// initial mouse position while staying outside the trigger's edge, keyboard
// opens center on the trigger, and either flips below when needed.
export function HoverTip({
  summary,
  children,
  className = "",
}: {
  summary: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const { triggerRef, popoverRef, show, hide } = useHoverPopover<
    HTMLSpanElement,
    HTMLSpanElement
  >();

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: the WAI tooltip pattern hangs hover and focus reveal off a non-interactive trigger; the handlers only toggle the card.
    <span
      className={`hover-tip ${className}`.trim()}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: the WAI tooltip pattern needs a focusable trigger so the card is reachable by keyboard, and the trigger itself performs no action.
      tabIndex={0}
      ref={triggerRef}
      onMouseEnter={(event) => show(event)}
      onMouseLeave={hide}
      onFocus={() => show()}
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
        ref={popoverRef}
      >
        {children}
      </span>
    </span>
  );
}
