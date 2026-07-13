import { type ReactNode, useEffect, useState } from "react";

import { useReducedMotion } from "./primitives";

// Tab strip: ARIA tablist/tab/tabpanel, matching the pre-React insights.js
// keyboard contract (ArrowRight/Down next, ArrowLeft/Up prev, Home/End,
// click select). One shared component drives Velocity/Tools/Health/
// Economics; each mounts its own instance keyed by a stable id prefix.

export type Tab = { id: string; label: string };

// useTabState resets to defaultId whenever resetKey changes: the old page
// reset every tab strip to "All instruments" on a range swap (the markup
// lived inside the swapped section), so a range change here must not
// preserve a mid-session tab choice either.
export function useTabState(defaultId: string, resetKey: unknown) {
  const [active, setActive] = useState(defaultId);
  // biome-ignore lint/correctness/useExhaustiveDependencies: resetKey is a deliberate trigger-only probe (the range identity) that this effect never reads; the reset must still fire whenever it changes, which is the entire point of the hook.
  useEffect(() => {
    setActive(defaultId);
  }, [defaultId, resetKey]);
  return [active, setActive] as const;
}

export function TabStrip({
  id,
  ariaLabel,
  tabs,
  active,
  onSelect,
}: {
  id: string;
  ariaLabel: string;
  tabs: Tab[];
  active: string;
  onSelect: (id: string) => void;
}) {
  return (
    <div role="tablist" aria-label={ariaLabel} className="tabstrip" id={id}>
      {tabs.map((t, i) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          id={`${id}-tab-${t.id}`}
          aria-controls={`${id}-panel-${t.id}`}
          aria-selected={active === t.id}
          tabIndex={active === t.id ? 0 : -1}
          onClick={() => onSelect(t.id)}
          onKeyDown={(e) => {
            let next: Tab | null = null;
            if (e.key === "ArrowRight" || e.key === "ArrowDown")
              next = tabs[(i + 1) % tabs.length] ?? null;
            else if (e.key === "ArrowLeft" || e.key === "ArrowUp")
              next = tabs[(i - 1 + tabs.length) % tabs.length] ?? null;
            else if (e.key === "Home") next = tabs[0] ?? null;
            else if (e.key === "End") next = tabs[tabs.length - 1] ?? null;
            if (next) {
              e.preventDefault();
              onSelect(next.id);
              document.getElementById(`${id}-tab-${next.id}`)?.focus();
            }
          }}
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}

// TabPanel keeps every panel mounted (native `hidden`, not an unmount), so a
// panel that measures its own container width on mount (the churn treemap)
// sees a real ResizeObserver callback fire when the tab is selected and the
// panel's box goes from 0 to its laid-out size, instead of losing its state
// (the treemap's drill path) on every tab switch.
export function TabPanel({
  stripId,
  tabId,
  active,
  children,
}: {
  stripId: string;
  tabId: string;
  active: string;
  children: ReactNode;
}) {
  const selected = active === tabId;
  return (
    <div
      role="tabpanel"
      id={`${stripId}-panel-${tabId}`}
      aria-labelledby={`${stripId}-tab-${tabId}`}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: the WAI-ARIA tabpanel pattern calls for a focusable panel root so keyboard users can move focus straight from the selected tab into its content in one step.
      tabIndex={0}
      hidden={!selected}
      className={selected ? "tabpanel tabpanel-fade" : "tabpanel"}
    >
      {children}
    </div>
  );
}

// MiniMultipleButton is an "All instruments" overview tile: clicking it jumps
// the parent tab strip to the full instrument and scrolls the section into
// view, honoring prefers-reduced-motion.
export function MiniMultipleButton({
  onJump,
  scrollTargetId,
  children,
}: {
  onJump: () => void;
  scrollTargetId: string;
  children: ReactNode;
}) {
  const reduced = useReducedMotion();
  return (
    <button
      type="button"
      className="mini-multiple"
      onClick={() => {
        onJump();
        document.getElementById(scrollTargetId)?.scrollIntoView({
          behavior: reduced ? "instant" : "smooth",
          block: "start",
        });
      }}
    >
      {children}
    </button>
  );
}

export function ChartCaption({
  title,
  value,
}: {
  title: string;
  value: string;
}) {
  return (
    <div className="chart-caption">
      <span className="chart-title">{title}</span>
      <span className="chart-value">{value}</span>
    </div>
  );
}
