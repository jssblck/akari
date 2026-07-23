import type { Insights, Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import { fmtS } from "./format";
import {
  AxisBaseline,
  AxisTicksY,
  BucketAxis,
  ChartSvg,
  ClipRect,
  HoverBucket,
  pathBand,
  pathLine,
  scaleLinear,
  TooltipRow,
  TooltipTitle,
  useClipId,
} from "./primitives";
import {
  ChartCaption,
  MiniMultipleButton,
  TabPanel,
  TabStrip,
  useTabState,
} from "./tabs";
import { useChartTooltip } from "./tooltip";

const W = 1000;
const H = 380;
const padL = 40;
const padR = 14;
const padT = 14;
const padB = 26;
const MW = 420;
const MH = 210;
const mpadL = 28;
const mpadR = 8;
const mpadT = 8;
const mpadB = 18;

const DOW = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

function ActiveHoursChart({ trends, mini }: { trends: Trends; mini: boolean }) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? mpadL : padL;
  const pR = mini ? mpadR : padR;
  const pT = mini ? mpadT : padT;
  const pB = mini ? mpadB : padB;
  const V = trends.Velocity;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const maxV = Math.max(...V.WallHours, 0) * 1.1 || 1;
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const bw = (w - pL - pR) / Math.max(n, 1);
  const wallPts = V.WallHours.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );

  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={mini ? [0, Math.round(maxV)] : [0, 15, 30, 45]}
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => String(v)}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={mini}
        n={n}
        labels={trends.Labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {V.ActiveHours.map((v, i) => {
          const x = xScale(i) - bw * 0.32;
          const y = yScale(v);
          return (
            <rect
              // biome-ignore lint/suspicious/noArrayIndexKey: bucket index is the stable key
              key={i}
              x={x}
              y={y}
              width={bw * 0.64}
              height={h - pB - y}
              fill="var(--viz-8)"
              opacity={0.78}
            />
          );
        })}
        <path
          d={pathLine(wallPts)}
          fill="none"
          stroke="var(--muted)"
          strokeWidth={mini ? 1 : 1.4}
          strokeDasharray="3,3"
        />
      </ClipRect>
      <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
      {!mini && (
        <HoverBucket
          w={w}
          h={h}
          pL={pL}
          pR={pR}
          pT={pT}
          pB={pB}
          n={n}
          xScale={xScale}
          tooltip={(i) => (
            <>
              <TooltipTitle>{trends.Labels[i]}</TooltipTitle>
              <TooltipRow color="var(--viz-8)">
                active h <b>{(V.ActiveHours[i] ?? 0).toFixed(1)}</b>
              </TooltipRow>
              <TooltipRow color="var(--muted)">
                wall-clock span h <b>{(V.WallHours[i] ?? 0).toFixed(1)}</b>
              </TooltipRow>
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function ResponseTimeChart({
  trends,
  mini,
}: {
  trends: Trends;
  mini: boolean;
}) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? mpadL : padL;
  const pR = mini ? mpadR : padR;
  const pT = mini ? mpadT : padT;
  const pB = mini ? mpadB : padB;
  const RT = trends.Velocity;
  const drawn = mini
    ? RT.ResponseP50.concat(RT.ResponseP90)
    : RT.ResponseP50.concat(RT.ResponseP90, RT.ResponseP99);
  const dataMax = drawn.length ? Math.max(...drawn) : 0;
  const yMax = Math.max(mini ? 40 : 120, Math.ceil((dataMax * 1.1) / 10) * 10);
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([0, yMax], [h - pB, pT]);
  const yticks = mini
    ? [0, yMax]
    : [0, yMax * 0.25, yMax * 0.5, yMax * 0.75, yMax].map((v) => Math.round(v));
  const p50Pts = RT.ResponseP50.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const p90Pts = RT.ResponseP90.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const p99Pts = RT.ResponseP99.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );

  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={yticks}
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => String(v)}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={mini}
        n={n}
        labels={trends.Labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        <path d={pathBand(p90Pts, p50Pts)} fill="var(--viz-2)" opacity={0.16} />
        <path
          d={pathLine(p50Pts)}
          fill="none"
          stroke="var(--viz-2)"
          strokeWidth={mini ? 1.4 : 2}
        />
        {!mini && (
          <>
            <path
              d={pathLine(p90Pts)}
              fill="none"
              stroke="var(--viz-2)"
              strokeWidth={1}
              opacity={0.5}
              strokeDasharray="2,3"
            />
            <path
              d={pathLine(p99Pts)}
              fill="none"
              stroke="var(--warn)"
              strokeWidth={1.4}
              strokeDasharray="4,3"
            />
          </>
        )}
      </ClipRect>
      <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
      {!mini && (
        <HoverBucket
          w={w}
          h={h}
          pL={pL}
          pR={pR}
          pT={pT}
          pB={pB}
          n={n}
          xScale={xScale}
          tooltip={(i) => (
            <>
              <TooltipTitle>{trends.Labels[i]}</TooltipTitle>
              <TooltipRow color="var(--viz-2)">
                p50 <b>{fmtS(RT.ResponseP50[i] ?? 0)}</b>
              </TooltipRow>
              <TooltipRow color="var(--muted)">
                p90 <b>{fmtS(RT.ResponseP90[i] ?? 0)}</b>
              </TooltipRow>
              <TooltipRow color="var(--warn)">
                p99 <b>{fmtS(RT.ResponseP99[i] ?? 0)}</b>
              </TooltipRow>
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function ThroughputChart({ trends, mini }: { trends: Trends; mini: boolean }) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? mpadL : padL;
  const pR = mini ? mpadR : padR;
  const pT = mini ? mpadT : padT;
  const pB = mini ? mpadB : padB;
  const T = trends.Velocity;
  const allVals = T.MsgsPerMin.concat(T.ToolsPerMin);
  const maxV = Math.max(1, Math.max(...allVals, 0)) * 1.15;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const series = [
    {
      key: "MsgsPerMin" as const,
      label: "msgs/min",
      color: "var(--viz-2)",
      width: mini ? 1.4 : 2,
    },
    {
      key: "ToolsPerMin" as const,
      label: "tools/min",
      color: "var(--viz-5)",
      width: mini ? 1.2 : 1.7,
    },
  ];
  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={
          mini
            ? [0, +maxV.toFixed(1)]
            : [0, +(maxV / 2).toFixed(1), +maxV.toFixed(1)]
        }
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => String(v)}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={mini}
        n={n}
        labels={trends.Labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {series.map((s) => (
          <path
            key={s.key}
            d={pathLine(T[s.key].map((v, i) => [xScale(i), yScale(v)]))}
            fill="none"
            stroke={s.color}
            strokeWidth={s.width}
          />
        ))}
      </ClipRect>
      <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
      {!mini && (
        <HoverBucket
          w={w}
          h={h}
          pL={pL}
          pR={pR}
          pT={pT}
          pB={pB}
          n={n}
          xScale={xScale}
          tooltip={(i) => (
            <>
              <TooltipTitle>{trends.Labels[i]}</TooltipTitle>
              {series.map((s) => (
                <TooltipRow color={s.color} key={s.key}>
                  {s.label} <b>{(T[s.key][i] ?? 0).toFixed(1)}</b>
                </TooltipRow>
              ))}
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function mixPunchColor(t: number): string {
  const base: [number, number, number] = [0x24, 0x22, 0x28];
  const target: [number, number, number] = [0xc6, 0xa8, 0xf2];
  const c = base.map((v, i) => Math.round(v + ((target[i] ?? v) - v) * t));
  return `rgb(${c.join(",")})`;
}

function PunchcardChart({ trends, mini }: { trends: Trends; mini: boolean }) {
  const w = mini ? MW : 1000;
  const h = mini ? MH : 320;
  const cols = 24;
  const rows = 7;
  const pL = mini ? 22 : 44;
  const pR = mini ? 4 : 14;
  const pT = mini ? 4 : 10;
  const pB = mini ? 14 : 26;
  const gap = mini ? 1 : 2;
  const cellW = (w - pL - pR) / cols;
  const cellH = (h - pT - pB) / rows;
  const cellSize = Math.min(cellW, cellH) - gap;
  const cells = trends.Rhythm.Cells;
  const { show, hide } = useChartTooltip();
  let maxVol = 0;
  for (const row of cells) for (const v of row) if (v > maxVol) maxVol = v;

  return (
    <ChartSvg w={w} h={h}>
      {!mini &&
        cells.map((_, r) => (
          <text
            // biome-ignore lint/suspicious/noArrayIndexKey: row index is the stable key
            key={r}
            x={pL - 8}
            y={pT + r * cellH + cellH / 2 + 3}
            className="punchcard-row-label"
            textAnchor="end"
          >
            {DOW[r]}
          </text>
        ))}
      {cells.map((row, r) =>
        row.map((v, c) => {
          const t = maxVol > 0 ? v / maxVol : 0;
          const cx = pL + c * cellW + cellW / 2;
          const cy = pT + r * cellH + cellH / 2;
          return (
            // biome-ignore lint/a11y/noStaticElementInteractions: mouse-only hover tooltip on a punchcard cell; the same volume figures drive the fill color, which is legible on its own.
            <rect
              // biome-ignore lint/suspicious/noArrayIndexKey: (row, col) is the day-of-week/hour-of-day grid coordinate itself, a fixed 7x24 layout that never reorders.
              key={`${r}-${c}`}
              x={cx - cellSize / 2}
              y={cy - cellSize / 2}
              width={cellSize}
              height={cellSize}
              rx={mini ? 1 : 2}
              fill={mixPunchColor(t)}
              className="scatter-dot"
              onMouseMove={
                mini
                  ? undefined
                  : (e) =>
                      show(
                        e.clientX,
                        e.clientY,
                        <>
                          <TooltipTitle>
                            {DOW[r]} {String(c).padStart(2, "0")}:00
                          </TooltipTitle>
                          <TooltipRow>
                            volume <b>{v.toLocaleString()}</b>
                          </TooltipRow>
                        </>,
                      )
              }
              onMouseLeave={mini ? undefined : hide}
            />
          );
        }),
      )}
      {!mini &&
        [0, 6, 12, 18, 23].map((c) => (
          <text
            key={c}
            x={pL + c * cellW + cellW / 2}
            y={h - pB + 16}
            className="axis-tick-text"
            textAnchor="middle"
          >
            {String(c).padStart(2, "0")}:00
          </text>
        ))}
    </ChartSvg>
  );
}

function punchcardPeak(cells: number[][]): string {
  let bestD = -1;
  let bestH = -1;
  let best = 0;
  cells.forEach((row, d) => {
    row.forEach((v, h) => {
      if (v > best) {
        best = v;
        bestD = d;
        bestH = h;
      }
    });
  });
  if (bestD < 0) return "";
  return `peak ${DOW[bestD]} ${String(bestH).padStart(2, "0")}:00`;
}

const VTABS = [
  { id: "all", label: "All instruments" },
  { id: "activehours", label: "Agent hours" },
  { id: "response", label: "Response time" },
  { id: "throughput", label: "Throughput" },
  { id: "rhythm", label: "Rhythm" },
];

export function VelocityInstrument({
  insights,
  trends,
  resetKey,
}: {
  insights: Insights;
  trends: Trends;
  resetKey: unknown;
}) {
  const [active, setActive] = useTabState("all", resetKey);
  const V = trends.Velocity;
  const avgActive = V.ActiveHours.length
    ? V.ActiveHours.reduce((a, b) => a + b, 0) / V.ActiveHours.length
    : 0;
  const lastActive = V.ActiveHours[V.ActiveHours.length - 1] ?? 0;
  const lastP50 = V.ResponseP50[V.ResponseP50.length - 1] ?? 0;
  const lastMsgs = V.MsgsPerMin[V.MsgsPerMin.length - 1] ?? 0;
  const peakLabel = punchcardPeak(trends.Rhythm.Cells);

  return (
    <section className="instrument" id="velocity" aria-labelledby="velocity-h">
      <div className="instrument-head">
        <h2 id="velocity-h">Velocity</h2>
      </div>
      <div className="panel">
        <TabStrip
          id="velocity-tabs"
          ariaLabel="Velocity instruments"
          tabs={VTABS}
          active={active}
          onSelect={setActive}
        />
        <TabPanel stripId="velocity-tabs" tabId="all" active={active}>
          <div className="grid-2x2">
            <MiniMultipleButton
              onJump={() => setActive("activehours")}
              scrollTargetId="velocity"
            >
              <ChartCaption
                title="Agent hours"
                value={`${lastActive.toFixed(1)}h today`}
              />
              <ActiveHoursChart trends={trends} mini />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("response")}
              scrollTargetId="velocity"
            >
              <ChartCaption
                title="Response time"
                value={`${fmtS(lastP50)} p50`}
              />
              <ResponseTimeChart trends={trends} mini />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("throughput")}
              scrollTargetId="velocity"
            >
              <ChartCaption
                title="Throughput"
                value={`${lastMsgs.toFixed(1)} msgs/min`}
              />
              <ThroughputChart trends={trends} mini />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("rhythm")}
              scrollTargetId="velocity"
            >
              <ChartCaption title="Rhythm" value={peakLabel} />
              <PunchcardChart trends={trends} mini />
            </MiniMultipleButton>
          </div>
        </TabPanel>
        <TabPanel stripId="velocity-tabs" tabId="activehours" active={active}>
          <StatStrip>
            <Stat label="avg active h/day" value={`${avgActive.toFixed(1)}h`} />
            <Stat
              label="peak concurrent sessions"
              value={insights.Concurrency.FleetPeak.toLocaleString()}
            />
            <Stat
              label="avg concurrent sessions"
              value={insights.Concurrency.AvgConcurrent.toFixed(1)}
            />
          </StatStrip>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ActiveHoursChart trends={trends} mini={false} />
            </div>
          </div>
        </TabPanel>
        <TabPanel stripId="velocity-tabs" tabId="response" active={active}>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ResponseTimeChart trends={trends} mini={false} />
            </div>
          </div>
        </TabPanel>
        <TabPanel stripId="velocity-tabs" tabId="throughput" active={active}>
          <StatStrip>
            <Stat
              label="avg msgs/active min"
              value={insights.Velocity.MsgsPerActiveMin.toFixed(1)}
            />
            <Stat
              label="avg tools/active min"
              value={insights.Velocity.ToolsPerActiveMin.toFixed(1)}
            />
          </StatStrip>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ThroughputChart trends={trends} mini={false} />
            </div>
          </div>
        </TabPanel>
        <TabPanel stripId="velocity-tabs" tabId="rhythm" active={active}>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <PunchcardChart trends={trends} mini={false} />
            </div>
          </div>
        </TabPanel>
      </div>
    </section>
  );
}
