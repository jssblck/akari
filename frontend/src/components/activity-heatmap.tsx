import { useLayoutEffect, useMemo, useRef, useState } from "react";

import { formatCost, formatTokens } from "../format";
import type { DayPoint } from "../types";

export type HeatmapMetric = "tokens" | "cost";

const DAY_MS = 86400000;
// Trailing columns, ~1 year ending on the current week. The window the range
// tabs select changes which days carry data; the grid shape stays fixed so the
// eye can compare windows against the same canvas.
const WEEKS = 53;
const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

type DayRecord = {
  cost: number;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
};

// Server day buckets are UTC-truncated, so the grid keys days in UTC to line
// up with them.
function keyOf(ts: number): string {
  const d = new Date(ts);
  const m = d.getUTCMonth() + 1;
  const day = d.getUTCDate();
  return `${d.getUTCFullYear()}-${m < 10 ? `0${m}` : m}-${day < 10 ? `0${day}` : day}`;
}

function formatFullDay(ts: number): string {
  const d = new Date(ts);
  return `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()}, ${d.getUTCFullYear()}`;
}

function tokensOf(rec: DayRecord): number {
  return rec.input + rec.output + rec.cacheRead + rec.cacheWrite;
}

function valueFor(rec: DayRecord | undefined, metric: HeatmapMetric): number {
  if (!rec) return 0;
  return metric === "cost" ? rec.cost : tokensOf(rec);
}

// levelFor maps a value to one of five steps (0 empty, 1-4 ramp). A sqrt scale
// lifts the long tail of small days off the floor so they remain legible.
function levelFor(v: number, max: number): number {
  if (v <= 0 || max <= 0) return 0;
  const f = Math.sqrt(v / max);
  return f > 0.75 ? 4 : f > 0.5 ? 3 : f > 0.25 ? 2 : 1;
}

type TooltipState = {
  ts: number;
  rec: DayRecord | undefined;
  cellX: number;
  cellY: number;
};

export function ActivityHeatmap({
  series,
  metric,
}: {
  series: DayPoint[] | null;
  metric: HeatmapMetric;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const tooltipRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(720);
  const [tooltip, setTooltip] = useState<TooltipState | null>(null);
  const [tooltipPos, setTooltipPos] = useState({ left: 0, top: 0 });

  const index = useMemo(() => {
    const idx = new Map<string, DayRecord>();
    for (const point of series ?? []) {
      idx.set(point.Day.slice(0, 10), {
        cost: point.CostUSD,
        input: point.Input,
        output: point.Output,
        cacheRead: point.CacheRead,
        cacheWrite: point.CacheWrite,
      });
    }
    return idx;
  }, [series]);

  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const measure = () => setWidth(el.clientWidth || 720);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // The grid ends on the current week and spans WEEKS columns back to a Sunday.
  const { start, end } = useMemo(() => {
    // Today must be the current UTC calendar day, matching the server's
    // UTC-truncated buckets; local date parts would lag a day behind for
    // negative-offset viewers and drop today's cell.
    const now = new Date();
    const end = Date.UTC(
      now.getUTCFullYear(),
      now.getUTCMonth(),
      now.getUTCDate(),
    );
    const endDow = new Date(end).getUTCDay();
    return { start: end - endDow * DAY_MS - (WEEKS - 1) * 7 * DAY_MS, end };
  }, []);

  const gap = 3;
  const labelH = 16;
  const padT = 2;
  // Floor the cell at a size a finger can still tell apart. When the floor
  // binds (a phone can't fit a year of readable cells), the grid keeps its
  // natural width and the container pans instead, parked on the most recent
  // weeks after render.
  const cell = Math.max(9, Math.floor((width - (WEEKS - 1) * gap) / WEEKS));
  const gridW = WEEKS * cell + (WEEKS - 1) * gap;
  const scrolls = gridW > width;
  const gridH = 7 * cell + 6 * gap;
  const height = padT + gridH + labelH;

  // Peak metric value across the visible window sets the ramp ceiling.
  const max = useMemo(() => {
    let peak = 0;
    for (let c = 0; c < WEEKS; c++) {
      for (let r = 0; r < 7; r++) {
        const v = valueFor(
          index.get(keyOf(start + (c * 7 + r) * DAY_MS)),
          metric,
        );
        if (v > peak) peak = v;
      }
    }
    return peak;
  }, [index, metric, start]);

  // Park a panning grid on its trailing edge, where the most recent weeks are.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (el && scrolls) el.scrollLeft = el.scrollWidth;
  }, [scrolls]);

  // Tooltip placement needs the rendered tooltip's size, so it positions in a
  // layout effect after the content committed: centered above the cell, clamped
  // to the pannable content, flipped below near the top edge.
  useLayoutEffect(() => {
    const el = containerRef.current;
    const tip = tooltipRef.current;
    if (!el || !tip || !tooltip) return;
    const tw = tip.offsetWidth;
    const th = tip.offsetHeight;
    let left = tooltip.cellX + cell / 2 - tw / 2;
    const bound = (scrolls ? el.scrollWidth : el.clientWidth) - tw;
    left = Math.max(0, Math.min(left, bound));
    let top = tooltip.cellY - th - 8;
    if (top < 0) top = tooltip.cellY + cell + 8;
    setTooltipPos({ left, top });
  }, [tooltip, cell, scrolls]);

  if (index.size === 0)
    return <div className="chart-empty">No dated usage to chart.</div>;

  const cells = [];
  for (let col = 0; col < WEEKS; col++) {
    for (let row = 0; row < 7; row++) {
      const ts = start + (col * 7 + row) * DAY_MS;
      if (ts > end) continue; // future days in the current week stay blank
      const rec = index.get(keyOf(ts));
      const lvl = levelFor(valueFor(rec, metric), max);
      const x = col * (cell + gap);
      const y = padT + row * (cell + gap);
      cells.push(
        <rect
          key={ts}
          className={`hm-cell lvl-${lvl}`}
          x={x}
          y={y}
          width={cell}
          height={cell}
          rx={2}
          ry={2}
        />,
      );
    }
  }

  // One hit-testing handler on the svg instead of a listener per cell: map the
  // pointer to its column and row, ignore the gaps between cells and the blank
  // future days, and only touch state when the hovered day actually changes.
  const onMouseMove = (event: React.MouseEvent<SVGSVGElement>) => {
    const { offsetX, offsetY } = event.nativeEvent;
    const pitch = cell + gap;
    const col = Math.floor(offsetX / pitch);
    const row = Math.floor((offsetY - padT) / pitch);
    if (col < 0 || col >= WEEKS || row < 0 || row >= 7) return;
    if (offsetX % pitch >= cell || (offsetY - padT) % pitch >= cell) return;
    const ts = start + (col * 7 + row) * DAY_MS;
    if (ts > end) return;
    if (tooltip?.ts === ts) return;
    setTooltip({
      ts,
      rec: index.get(keyOf(ts)),
      cellX: col * pitch,
      cellY: padT + row * pitch,
    });
  };

  // Month labels along the bottom, placed at the column where a month begins,
  // spaced at least three columns apart so short months never collide.
  const labels = [];
  let lastMonth = -1;
  let lastLabelCol = -2;
  for (let col = 0; col < WEEKS; col++) {
    const month = new Date(start + col * 7 * DAY_MS).getUTCMonth();
    if (month !== lastMonth && col - lastLabelCol >= 3) {
      labels.push(
        <text
          key={col}
          className="hm-month"
          x={col * (cell + gap)}
          y={height - 4}
        >
          {MONTHS[month]}
        </text>,
      );
      lastLabelCol = col;
    }
    lastMonth = month;
  }

  const total = tooltip?.rec ? tokensOf(tooltip.rec) : 0;
  return (
    <div
      ref={containerRef}
      className={scrolls ? "heatmap hm-scroll" : "heatmap"}
    >
      <svg
        viewBox={`0 0 ${gridW} ${height}`}
        width={gridW}
        height={height}
        role="img"
        aria-label="Daily activity"
        onMouseMove={onMouseMove}
        onMouseLeave={() => setTooltip(null)}
      >
        {cells}
        {labels}
      </svg>
      <div
        ref={tooltipRef}
        className={tooltip ? "heatmap-tooltip on" : "heatmap-tooltip"}
        style={{ left: tooltipPos.left, top: tooltipPos.top }}
        aria-hidden="true"
      >
        {tooltip ? (
          <>
            <div className="tt-day">{formatFullDay(tooltip.ts)}</div>
            {!tooltip.rec || total === 0 ? (
              <div className="tt-empty">No activity</div>
            ) : (
              <>
                <div className="tt-total">{formatTokens(total)} tokens</div>
                <dl className="tt-grid">
                  <dt>In</dt>
                  <dd>{formatTokens(tooltip.rec.input)}</dd>
                  <dt>Out</dt>
                  <dd>{formatTokens(tooltip.rec.output)}</dd>
                  <dt>Cache read</dt>
                  <dd>{formatTokens(tooltip.rec.cacheRead)}</dd>
                  <dt>Cache write</dt>
                  <dd>{formatTokens(tooltip.rec.cacheWrite)}</dd>
                </dl>
                <div className="tt-cost">{formatCost(tooltip.rec.cost)}</div>
              </>
            )}
          </>
        ) : null}
      </div>
    </div>
  );
}
