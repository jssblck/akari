import {
  type ReactNode,
  useCallback,
  useEffect,
  useId,
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
  const pinnedRef = useRef(false);
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
    pinnedRef.current = false;
    setOpen(false);
  }, []);

  const hideTransient = useCallback(() => {
    if (!pinnedRef.current) hide();
  }, [hide]);

  const togglePinned = useCallback(
    (pointer?: PointerPosition) => {
      if (pinnedRef.current) {
        hide();
        return;
      }
      pinnedRef.current = true;
      show(pointer);
    },
    [hide, show],
  );

  // Manual popovers lack light-dismiss. Register the outside-pointer handler
  // only while a card is open, so closed tips add no document listener.
  useEffect(() => {
    if (!open) return;
    const dismissOutside = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (
        triggerRef.current?.contains(target) ||
        popoverRef.current?.contains(target)
      )
        return;
      hide();
    };
    document.addEventListener("pointerdown", dismissOutside, true);
    return () =>
      document.removeEventListener("pointerdown", dismissOutside, true);
  }, [hide, open]);

  // A top-layer card holds viewport coordinates, so scrolling would leave it
  // drifting away from its trigger; close it instead.
  useEffect(() => {
    if (!open) return;
    window.addEventListener("scroll", hide, true);
    return () => window.removeEventListener("scroll", hide, true);
  }, [open, hide]);

  return {
    triggerRef,
    popoverRef,
    open,
    show,
    hide,
    hideTransient,
    togglePinned,
  };
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
}: {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  reasoning?: number;
  costUSD?: number;
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
        <div className="tt-cost">{formatCost(costUSD)}</div>
      ) : null}
    </>
  );
}

// HoverTip wraps an inline figure with a card revealed by hover, keyboard
// focus, or a pinned tap. A second tap, an outside tap, Escape, blur, or scroll
// dismisses the card.
//
// The card is a native top-layer popover, so it always paints above the page
// above ancestor overflow and stacking contexts. Top-layer placement uses
// viewport coordinates: pointer opens align with the initial pointer position,
// keyboard opens center on the trigger, and either flips below when needed.
export function HoverTip({
  summary,
  children,
  className = "",
}: {
  summary: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const tipId = useId();
  const summaryId = useId();
  const {
    triggerRef,
    popoverRef,
    open,
    show,
    hide,
    hideTransient,
    togglePinned,
  } = useHoverPopover<HTMLSpanElement, HTMLSpanElement>();

  return (
    // biome-ignore lint/a11y/useSemanticElements: HoverTip is phrasing content and often sits inside a linked data row, where a nested button would be invalid HTML; keyboard and disclosure semantics are provided explicitly.
    <span
      className={`hover-tip ${className}`.trim()}
      role="button"
      tabIndex={0}
      ref={triggerRef}
      aria-controls={tipId}
      aria-expanded={open}
      aria-labelledby={summaryId}
      onMouseEnter={(event) => show(event)}
      onMouseLeave={hideTransient}
      onFocus={() => show()}
      onBlur={hide}
      onClick={(event) => {
        // A HoverTip can sit inside a linked data row. Tapping its disclosure
        // must not navigate the row out from under the card it just opened.
        event.preventDefault();
        event.stopPropagation();
        togglePinned(event);
      }}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          hide();
          return;
        }
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          event.stopPropagation();
          togglePinned();
        }
      }}
    >
      <span className="hover-tip-summary" id={summaryId}>
        {summary}
      </span>
      <span
        id={tipId}
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
