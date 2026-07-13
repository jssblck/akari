import {
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { useChartTooltip } from "./tooltip";

// Shared SVG chart engine, ported behavior-for-behavior from the pre-React
// insights.js (scaleLinear/scaleLog, the path builders, bucketAxis, the hover
// crosshair, and resolveLabelCollisions). Every chart in components/insights/
// composes these instead of reinventing axis or scale math, so the seven
// instruments read as one visual system.

export type Point = readonly [number, number];

export function scaleLinear(
  domain: readonly [number, number],
  range: readonly [number, number],
): (v: number) => number {
  const [d0, d1] = domain;
  const [r0, r1] = range;
  return (v: number) => {
    if (d1 === d0) return r0;
    return r0 + ((v - d0) / (d1 - d0)) * (r1 - r0);
  };
}

// scaleLog clamps input to 1e-6 before log10, so a zero or negative value
// (never expected on these domains, but a defensive floor) maps to the low
// end of the range instead of producing NaN.
export function scaleLog(
  domain: readonly [number, number],
  range: readonly [number, number],
): (v: number) => number {
  const [d0, d1] = domain;
  const [r0, r1] = range;
  const ld0 = Math.log10(Math.max(d0, 1e-6));
  const ld1 = Math.log10(Math.max(d1, 1e-6));
  return (v: number) => {
    const lv = Math.log10(Math.max(v, 1e-6));
    if (ld1 === ld0) return r0;
    return r0 + ((lv - ld0) / (ld1 - ld0)) * (r1 - r0);
  };
}

export function pathLine(points: Point[]): string {
  if (!points.length) return "";
  return points
    .map(
      (p, i) => `${i === 0 ? "M" : "L"}${p[0].toFixed(2)},${p[1].toFixed(2)}`,
    )
    .join(" ");
}

export function pathArea(points: Point[], baseline: number): string {
  if (!points.length) return "";
  const top = pathLine(points);
  // The length guard above proves both indices exist; the [0, 0] fallbacks
  // satisfy noUncheckedIndexedAccess without a non-null assertion and are
  // never actually reached.
  const last = points[points.length - 1] ?? [0, 0];
  const first = points[0] ?? [0, 0];
  return `${top} L${last[0].toFixed(2)},${baseline.toFixed(2)} L${first[0].toFixed(2)},${baseline.toFixed(2)} Z`;
}

// pathBand draws the shaded region between two series (a band, or a filled
// area when bottomPoints is a flat baseline), closing the shape by walking
// the bottom series in reverse.
export function pathBand(topPoints: Point[], bottomPoints: Point[]): string {
  const top = pathLine(topPoints);
  const rev = bottomPoints.slice().reverse();
  const bottom = rev
    .map((p) => `L${p[0].toFixed(2)},${p[1].toFixed(2)}`)
    .join(" ");
  return `${top} ${bottom} Z`;
}

// resolveLabelCollisions pushes overlapping right-edge value labels apart:
// sort by y, spread any pair closer than minGap, then clamp the whole
// resolved set back inside [top, bottom]. Shared by every chart that stacks
// labels on its right edge (Fleet mix, Throughput, Failures, Hygiene,
// Subagents).
export function resolveLabelCollisions<T extends { y: number }>(
  items: T[],
  minGap: number,
  top: number,
  bottom: number,
): T[] {
  const sorted = items.map((it) => ({ ...it }));
  sorted.sort((a, b) => a.y - b.y);
  // Every index touched below (i, i-1, 0, length-1) is proven in range by
  // the surrounding loop bounds and the sorted.length guard; the `if (cur &&
  // prev)` style checks document that invariant for the type checker
  // without asserting past it.
  for (let pass = 0; pass < sorted.length; pass++) {
    let moved = false;
    for (let i = 1; i < sorted.length; i++) {
      const cur = sorted[i];
      const prev = sorted[i - 1];
      if (!cur || !prev) continue;
      const gap = cur.y - prev.y;
      if (gap < minGap) {
        const push = (minGap - gap) / 2;
        prev.y -= push;
        cur.y += push;
        moved = true;
      }
    }
    if (!moved) break;
  }
  const firstItem = sorted[0];
  if (firstItem && firstItem.y < top) {
    const d = top - firstItem.y;
    for (const s of sorted) s.y += d;
  }
  const last = sorted[sorted.length - 1];
  if (last && last.y > bottom) {
    const d = last.y - bottom;
    for (const s of sorted) s.y -= d;
  }
  return sorted;
}

export function ChartSvg({
  w,
  h,
  className,
  children,
}: {
  w: number;
  h: number;
  className?: string;
  children: ReactNode;
}) {
  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      className={`chart-svg${className ? ` ${className}` : ""}`}
      preserveAspectRatio="none"
      role="presentation"
    >
      {children}
    </svg>
  );
}

// useClipId mints a stable id for a chart's plot-rect clipPath: every
// value-driven mark (bars, areas, lines, dots) renders inside the clipped
// group, so a point beyond the axis domain paints up to the plot edge and no
// further; axis chrome (ticks, labels) stays outside it in the page margins.
export function useClipId(): string {
  return `ins-clip-${useId()}`;
}

export function ClipRect({
  id,
  x,
  y,
  w,
  h,
  children,
}: {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  children: ReactNode;
}) {
  return (
    <>
      <clipPath id={id}>
        <rect x={x} y={y} width={Math.max(0, w)} height={Math.max(0, h)} />
      </clipPath>
      <g clipPath={`url(#${id})`}>{children}</g>
    </>
  );
}

export function AxisTicksY({
  values,
  xLeft,
  xRight,
  yScale,
  fmt,
}: {
  values: number[];
  xLeft: number;
  xRight: number;
  yScale: (v: number) => number;
  fmt?: (v: number) => string;
}) {
  return (
    <>
      {values.map((v) => {
        const y = yScale(v);
        return (
          <g key={v}>
            <line x1={xLeft} x2={xRight} y1={y} y2={y} className="gridline" />
            <text
              x={xLeft - 6}
              y={y + 3}
              className="axis-tick-text"
              textAnchor="end"
            >
              {fmt ? fmt(v) : String(v)}
            </text>
          </g>
        );
      })}
    </>
  );
}

export function AxisBaseline({
  x1,
  x2,
  y,
}: {
  x1: number;
  x2: number;
  y: number;
}) {
  return <line x1={x1} x2={x2} y1={y} y2={y} className="axis-line" />;
}

// BucketAxis draws the shared x-axis (first/middle/last tick, from the
// server-formatted Trends.Labels), suppressed entirely on the mini variant.
export function BucketAxis({
  w,
  h,
  pB,
  pL,
  pR,
  mini,
  n,
  labels,
}: {
  w: number;
  h: number;
  pB: number;
  pL: number;
  pR: number;
  mini: boolean;
  n: number;
  labels: string[];
}) {
  if (mini || n === 0) return null;
  const y = h - pB + 17;
  const marks = [0, Math.floor((n - 1) / 2), n - 1];
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  return (
    <>
      {marks.map((i) => (
        <text
          key={i}
          x={xScale(i)}
          y={y}
          className="axis-tick-text"
          textAnchor={i === 0 ? "start" : i === n - 1 ? "end" : "middle"}
        >
          {labels[i]}
        </text>
      ))}
    </>
  );
}

// HoverBucket lays an invisible hit rectangle over the whole plot and snaps
// the pointer to the nearest bucket index, drawing a crosshair and feeding
// the shared tooltip host. Full-variant charts only; minis render none of
// this.
export function HoverBucket({
  w,
  h,
  pL,
  pR,
  pT,
  pB,
  n,
  xScale,
  tooltip,
}: {
  w: number;
  h: number;
  pL: number;
  pR: number;
  pT: number;
  pB: number;
  n: number;
  xScale: (v: number) => number;
  tooltip: (index: number) => ReactNode;
}) {
  const { show, hide } = useChartTooltip();
  const [hoverX, setHoverX] = useState<number | null>(null);
  const indexScale = scaleLinear([pL, w - pR], [0, Math.max(n - 1, 0)]);

  return (
    <>
      {hoverX !== null && (
        <line
          x1={hoverX}
          x2={hoverX}
          y1={pT}
          y2={h - pB}
          stroke="var(--border-strong)"
          strokeWidth={1}
        />
      )}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: mouse-only hover crosshair over an SVG chart; every value it surfaces is already in the stat tiles and legend above, so there is no keyboard-only content behind it. */}
      <rect
        x={0}
        y={0}
        width={w}
        height={h}
        fill="transparent"
        onMouseMove={(e) => {
          const rect = e.currentTarget.getBoundingClientRect();
          const px = ((e.clientX - rect.left) / rect.width) * w;
          const i = Math.max(0, Math.min(n - 1, Math.round(indexScale(px))));
          const x = xScale(i);
          setHoverX(x);
          show(e.clientX, e.clientY, tooltip(i));
        }}
        onMouseLeave={() => {
          setHoverX(null);
          hide();
        }}
      />
    </>
  );
}

export function TooltipTitle({ children }: { children: ReactNode }) {
  return <div className="tt-title">{children}</div>;
}

export function TooltipRow({
  color,
  children,
}: {
  color?: string;
  children: ReactNode;
}) {
  return (
    <div className="tt-row" style={color ? { color } : undefined}>
      {children}
    </div>
  );
}

// useContainerSize measures an element's live pixel box via ResizeObserver,
// for the one chart on this page (the churn treemap) that lays out absolute-
// positioned HTML cells rather than an SVG viewBox that scales with CSS
// width. Every other chart here uses a fixed viewBox + width:100%, which
// needs no JS measurement.
export function useContainerSize<T extends HTMLElement>(
  fallback: { width: number; height: number } = { width: 700, height: 420 },
) {
  const ref = useRef<T>(null);
  const [size, setSize] = useState(fallback);
  // The default fallback object literal is re-created every render; reading
  // it through a ref (rather than the effect's closure) lets the effect keep
  // an empty dependency array without either re-registering the observer on
  // every render or going stale if a caller passes a genuinely changing one.
  const fallbackRef = useRef(fallback);
  fallbackRef.current = fallback;

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () =>
      setSize({
        width: el.clientWidth || fallbackRef.current.width,
        height: el.clientHeight || fallbackRef.current.height,
      });
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  return { ref, size };
}

// useReducedMotion reads prefers-reduced-motion once and stays in sync with
// live changes, gating the tabpanel fade-in and the jump buttons' smooth
// scroll.
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(
    () =>
      window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false,
  );
  useEffect(() => {
    const mql = window.matchMedia("(prefers-reduced-motion: reduce)");
    const onChange = () => setReduced(mql.matches);
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, []);
  return reduced;
}
