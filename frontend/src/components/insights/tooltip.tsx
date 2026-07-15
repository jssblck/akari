import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";

// One shared, fixed-position tooltip host serves every chart under a
// TooltipHost (the old insights.js kept a single #tooltip node for the same
// reason: mounting one per chart would fight over z-index and hover state).
// Content is built as JSX by the caller, never HTML, so there is no
// injection surface to sanitize the way the old client-side allowlist did.

type TooltipState = { x: number; y: number; content: ReactNode } | null;

const TooltipCtx = createContext<{
  show: (clientX: number, clientY: number, content: ReactNode) => void;
  hide: () => void;
} | null>(null);

export function useChartTooltip() {
  const ctx = useContext(TooltipCtx);
  if (!ctx)
    throw new Error("useChartTooltip must be used within a TooltipHost");
  return ctx;
}

export function TooltipHost({ children }: { children: ReactNode }) {
  const [state, setState] = useState<TooltipState>(null);
  const nodeRef = useRef<HTMLDivElement>(null);

  const show = useCallback(
    (clientX: number, clientY: number, content: ReactNode) => {
      setState({ x: clientX, y: clientY, content });
    },
    [],
  );
  const hide = useCallback(() => setState(null), []);

  const ctx = useMemo(() => ({ show, hide }), [show, hide]);

  // Position clamped to the viewport after paint, mirroring the old
  // showTooltip: offset from the pointer, flipped to the other side when it
  // would run off the right or bottom edge.
  let style: React.CSSProperties = { display: "none" };
  if (state) {
    const pad = 14;
    let left = state.x + pad;
    let top = state.y + pad;
    const node = nodeRef.current;
    if (node) {
      const rect = node.getBoundingClientRect();
      if (left + rect.width > window.innerWidth - 8)
        left = state.x - rect.width - pad;
      if (top + rect.height > window.innerHeight - 8)
        top = state.y - rect.height - pad;
    }
    style = { display: "block", left, top };
  }

  return (
    <TooltipCtx.Provider value={ctx}>
      {children}
      <div
        ref={nodeRef}
        className="chart-tooltip popover"
        role="status"
        style={style}
      >
        {state?.content}
      </div>
    </TooltipCtx.Provider>
  );
}
