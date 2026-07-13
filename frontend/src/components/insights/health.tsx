import { formatTokens } from "../../format";
import type { Insights, SignalTrends, Trends } from "../../types";
import { InstrumentCaption } from "./caption";
import {
  ARCHETYPE_ORDER,
  archetypeColor,
  archetypeLabel,
  GRADE_ORDER,
  gradeColor,
  gradeLabel,
} from "./format";
import { Legend } from "./legend";
import {
  AxisBaseline,
  AxisTicksY,
  BucketAxis,
  ChartSvg,
  ClipRect,
  HoverBucket,
  pathArea,
  pathBand,
  pathLine,
  resolveLabelCollisions,
  scaleLinear,
  scaleLog,
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

const W = 1000;
const H = 380;
const padL = 40;
const padR = 46;
const padT = 14;
const padB = 26;
const MW = 420;
const MH = 210;

function gradeShare(signals: SignalTrends, i: number, key: string): number {
  if (key === "U") return signals.GradeShare[i]?.[""] ?? 0;
  return signals.GradeShare[i]?.[key] ?? 0;
}

// GradesChart draws the letter-grade stack with a secondary GPA line on the
// full variant; the mini strips everything but the bands, matching the old
// page's miniGrades cut.
export function GradesChart({
  signals,
  n,
  labels,
  mini,
}: {
  signals: SignalTrends;
  n: number;
  labels: string[];
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? 4 : padL;
  const pR = mini ? 4 : padR;
  const pT = mini ? 6 : padT;
  const pB = mini ? 6 : padB;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([0, 100], [h - pB, pT]);
  const gpaScale = scaleLinear([0, 4], [h - pB, pT]);
  let cum = new Array(n).fill(0);
  const bands = GRADE_ORDER.map((key) => {
    const bottom = cum.slice();
    const top = cum.map((c, i) => c + gradeShare(signals, i, key));
    cum = top;
    return {
      key,
      bottomPts: bottom.map(
        (v, i) => [xScale(i), yScale(v)] as [number, number],
      ),
      topPts: top.map((v, i) => [xScale(i), yScale(v)] as [number, number]),
    };
  });
  const gpaPts = signals.GPA.map(
    (v, i) => [xScale(i), gpaScale(v)] as [number, number],
  );
  const gpaNow = signals.GPA[signals.GPA.length - 1] ?? 0;
  const lastPt = gpaPts[gpaPts.length - 1];

  return (
    <ChartSvg w={w} h={h}>
      {!mini && (
        <>
          <AxisTicksY
            values={[0, 25, 50, 75, 100]}
            xLeft={pL}
            xRight={w - pR}
            yScale={yScale}
            fmt={(v) => `${v}%`}
          />
          <BucketAxis
            w={w}
            h={h}
            pB={pB}
            pL={pL}
            pR={pR}
            mini={false}
            n={n}
            labels={labels}
          />
        </>
      )}
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {bands.map((b) => (
          <path
            key={b.key}
            d={pathBand(b.topPts, b.bottomPts)}
            fill={gradeColor(b.key)}
            opacity={b.key === "U" ? 0.5 : 0.82}
          />
        ))}
        {!mini && (
          <path
            d={pathLine(gpaPts)}
            fill="none"
            stroke="var(--text)"
            strokeWidth={2}
          />
        )}
      </ClipRect>
      {!mini && (
        <>
          {[0, 1, 2, 3, 4].map((v) => (
            <text
              key={v}
              x={w - pR + 6}
              y={gpaScale(v) + 3}
              className="axis-tick-text"
              textAnchor="start"
            >
              {v.toFixed(0)}
            </text>
          ))}
          {lastPt && (
            <text
              x={w - pR + 6}
              y={lastPt[1] - 6}
              className="callout-label"
              fill="var(--text)"
            >
              GPA {gpaNow.toFixed(2)}
            </text>
          )}
          <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
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
                <TooltipTitle>{labels[i]}</TooltipTitle>
                {GRADE_ORDER.map((key) => (
                  <TooltipRow color={gradeColor(key)} key={key}>
                    {gradeLabel(key)}{" "}
                    <b>{gradeShare(signals, i, key).toFixed(1)}%</b>
                  </TooltipRow>
                ))}
                <TooltipRow>
                  GPA {signals.GPA[i]?.toFixed(2) ?? "0.00"}
                </TooltipRow>
              </>
            )}
          />
        </>
      )}
    </ChartSvg>
  );
}

// ArchetypesChart is not one of the fleet page's seven instruments (there is
// no archetype mount on /insights); it is drawn only by the project quality
// band, alongside Grades and Outcomes, matching the old renderQuality cut.
export function ArchetypesChart({
  signals,
  n,
  labels,
}: {
  signals: SignalTrends;
  n: number;
  labels: string[];
}) {
  const clipId = useClipId();
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [padL, W - padR]);
  const yScale = scaleLinear([0, 100], [H - padB, padT]);
  let cum = new Array(n).fill(0);
  const bands = ARCHETYPE_ORDER.map((key) => {
    const bottom = cum.slice();
    const top = cum.map((c, i) => c + (signals.ArchetypeShare[i]?.[key] ?? 0));
    cum = top;
    return {
      key,
      bottomPts: bottom.map(
        (v, i) => [xScale(i), yScale(v)] as [number, number],
      ),
      topPts: top.map((v, i) => [xScale(i), yScale(v)] as [number, number]),
    };
  });
  return (
    <>
      <ChartSvg w={W} h={H}>
        <AxisTicksY
          values={[0, 25, 50, 75, 100]}
          xLeft={padL}
          xRight={W - padR}
          yScale={yScale}
          fmt={(v) => `${v}%`}
        />
        <BucketAxis
          w={W}
          h={H}
          pB={padB}
          pL={padL}
          pR={padR}
          mini={false}
          n={n}
          labels={labels}
        />
        <ClipRect
          id={clipId}
          x={padL}
          y={padT}
          w={W - padL - padR}
          h={H - padT - padB}
        >
          {bands.map((b) => (
            <path
              key={b.key}
              d={pathBand(b.topPts, b.bottomPts)}
              fill={archetypeColor(b.key)}
              opacity={0.82}
            />
          ))}
        </ClipRect>
        <AxisBaseline x1={padL} x2={W - padR} y={H - padB} />
        <HoverBucket
          w={W}
          h={H}
          pL={padL}
          pR={padR}
          pT={padT}
          pB={padB}
          n={n}
          xScale={xScale}
          tooltip={(i) => (
            <>
              <TooltipTitle>{labels[i]}</TooltipTitle>
              {ARCHETYPE_ORDER.map((key) => (
                <TooltipRow color={archetypeColor(key)} key={key}>
                  {archetypeLabel(key)}{" "}
                  <b>{(signals.ArchetypeShare[i]?.[key] ?? 0).toFixed(1)}%</b>
                </TooltipRow>
              ))}
            </>
          )}
        />
      </ChartSvg>
      <Legend
        items={ARCHETYPE_ORDER.map((key) => ({
          color: archetypeColor(key),
          label: archetypeLabel(key),
        }))}
      />
    </>
  );
}

// abandMax picks a legible right-axis ceiling for the abandoned-rate line: a
// deliberate dual-axis reading (see the port spec's note on the old visual-
// offset trick) rather than sharing the completed-rate line's 60-100% axis,
// which would otherwise plot two rates on the same pixels under different
// meanings.
function abandMax(rates: number[]): number {
  const peak = Math.max(...rates, 0) * 1.15;
  return Math.max(20, Math.ceil(peak / 5) * 5);
}

export function OutcomesChart({
  signals,
  n,
  labels,
  mini,
}: {
  signals: SignalTrends;
  n: number;
  labels: string[];
  mini: boolean;
}) {
  const clipMiniId = useClipId();
  const clipLineId = useClipId();
  const clipBarId = useClipId();

  if (mini) {
    const w = MW;
    const h = MH;
    const pL = 4;
    const pR = 4;
    const pT = 8;
    const pB = 8;
    const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
    const yScale = scaleLinear([60, 100], [h - pB, pT]);
    const compPts = signals.CompletedRate.map(
      (v, i) => [xScale(i), yScale(v)] as [number, number],
    );
    return (
      <ChartSvg w={w} h={h}>
        <ClipRect id={clipMiniId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
          <path
            d={pathLine(compPts)}
            fill="none"
            stroke="var(--ok)"
            strokeWidth={1.6}
          />
        </ClipRect>
      </ChartSvg>
    );
  }

  const w = W;
  const h = H;
  const pL = padL;
  const pR = 16;
  const pT = padT;
  const pB = padB;
  const barH = h * 0.22;
  const lineH = h - barH - 10;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([60, 100], [lineH, pT]);
  const rightMax = abandMax(signals.AbandonedRate);
  const yScaleRight = scaleLinear([0, rightMax], [lineH, pT]);
  const compPts = signals.CompletedRate.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const abandPts = signals.AbandonedRate.map(
    (v, i) => [xScale(i), yScaleRight(v)] as [number, number],
  );

  const maxTotal = Math.max(...signals.OutcomeTotal, 1);
  const barsTop = lineH + 16;
  const barScale = scaleLinear([0, maxTotal], [0, barH - 16]);
  const bw = (w - pL - pR) / n - 2;

  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={[60, 70, 80, 90, 100]}
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => `${v}%`}
      />
      {[0, rightMax / 2, rightMax].map((v) => (
        <text
          key={v}
          x={w - pR + 6}
          y={yScaleRight(v) + 3}
          className="axis-tick-text"
          textAnchor="start"
        >
          {Math.round(v)}%
        </text>
      ))}
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={false}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipLineId} x={pL} y={pT} w={w - pL - pR} h={lineH - pT}>
        <path
          d={pathLine(compPts)}
          fill="none"
          stroke="var(--ok)"
          strokeWidth={2.2}
        />
        <path
          d={pathLine(abandPts)}
          fill="none"
          stroke="var(--warn)"
          strokeWidth={1.6}
          strokeDasharray="4,3"
        />
      </ClipRect>
      <AxisBaseline x1={pL} x2={w - pR} y={lineH} />
      <ClipRect id={clipBarId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {signals.OutcomeTotal.map((total, i) => {
          const completed = signals.CompletedCount[i] ?? 0;
          const abandoned = signals.AbandonedCount[i] ?? 0;
          const other = Math.max(0, total - completed - abandoned);
          const x = xScale(i) - bw / 2;
          const base = barsTop + (barH - 16);
          const hComp = barScale(completed);
          const hAband = barScale(abandoned);
          const hOther = barScale(other);
          return (
            <g key={labels[i]}>
              <rect
                x={x}
                y={base - hComp}
                width={bw}
                height={hComp}
                fill="var(--ok)"
                opacity={0.55}
              />
              <rect
                x={x}
                y={base - hComp - hAband}
                width={bw}
                height={hAband}
                fill="var(--warn)"
                opacity={0.55}
              />
              <rect
                x={x}
                y={base - hComp - hAband - hOther}
                width={bw}
                height={hOther}
                fill="var(--muted)"
                opacity={0.45}
              />
            </g>
          );
        })}
      </ClipRect>
      <HoverBucket
        w={w}
        h={h}
        pL={pL}
        pR={pR}
        pT={pT}
        pB={pB}
        n={n}
        xScale={xScale}
        tooltip={(i) => {
          const total = signals.OutcomeTotal[i] ?? 0;
          const other = Math.max(
            0,
            total -
              (signals.CompletedCount[i] ?? 0) -
              (signals.AbandonedCount[i] ?? 0),
          );
          return (
            <>
              <TooltipTitle>{labels[i]}</TooltipTitle>
              <TooltipRow color="var(--ok)">
                completed <b>{(signals.CompletedRate[i] ?? 0).toFixed(1)}%</b>
              </TooltipRow>
              <TooltipRow color="var(--warn)">
                abandoned <b>{(signals.AbandonedRate[i] ?? 0).toFixed(1)}%</b>
              </TooltipRow>
              <TooltipRow color="var(--muted)">
                other <b>{other}</b>
              </TooltipRow>
              <TooltipRow>sessions {total}</TooltipRow>
            </>
          );
        }}
      />
    </ChartSvg>
  );
}

const HYGIENE_SERIES = [
  {
    key: "HygieneTerse" as const,
    label: "Terse prompts",
    color: "var(--faint)",
  },
  {
    key: "HygieneRepeated" as const,
    label: "Repeated prompts",
    color: "var(--viz-2)",
  },
  {
    key: "HygieneNoCode" as const,
    label: "No code pointer",
    color: "var(--viz-3)",
  },
  {
    key: "HygieneUnstructured" as const,
    label: "Unstructured start",
    color: "var(--viz-8)",
  },
];

function HygieneChart({
  signals,
  n,
  labels,
  mini,
}: {
  signals: SignalTrends;
  n: number;
  labels: string[];
  mini: boolean;
}) {
  const clipId = useClipId();
  if (mini) {
    const w = MW;
    const h = MH;
    const pL = 6;
    const pR = 6;
    const pT = 8;
    const pB = 8;
    const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
    const yScale = scaleLinear([0, 10], [h - pB, pT]);
    const pts = signals.HygieneNoCode.map(
      (v, i) => [xScale(i), yScale(Math.min(v, 10))] as [number, number],
    );
    return (
      <ChartSvg w={w} h={h}>
        <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
          <path
            d={pathArea(pts, yScale(0))}
            fill="var(--viz-3)"
            opacity={0.2}
          />
          <path
            d={pathLine(pts)}
            fill="none"
            stroke="var(--viz-3)"
            strokeWidth={1.6}
          />
        </ClipRect>
        <AxisBaseline x1={pL} x2={w - pR} y={yScale(0)} />
      </ChartSvg>
    );
  }

  const w = W;
  const h = H;
  const pL = padL;
  const pR = 100;
  const pT = padT;
  const pB = padB;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([0, 10], [h - pB, pT]);
  const pendingLabels = HYGIENE_SERIES.map((s) => {
    const vals = signals[s.key];
    const last = vals[vals.length - 1] ?? 0;
    return {
      y: yScale(last),
      color: s.color,
      text: `${s.label} ${last.toFixed(1)}%`,
    };
  });

  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={[0, 2.5, 5, 7.5, 10]}
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => `${v}%`}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={false}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {HYGIENE_SERIES.map((s) => (
          <path
            key={s.key}
            d={pathLine(signals[s.key].map((v, i) => [xScale(i), yScale(v)]))}
            fill="none"
            stroke={s.color}
            strokeWidth={s.key === "HygieneNoCode" ? 2 : 1.5}
          />
        ))}
      </ClipRect>
      {resolveLabelCollisions(pendingLabels, 14, pT, h - pB).map((lbl) => (
        <text
          key={lbl.text}
          x={w - pR + 6}
          y={lbl.y + 3}
          className="callout-label"
          fill={lbl.color}
        >
          {lbl.text}
        </text>
      ))}
      <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
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
            <TooltipTitle>{labels[i]}</TooltipTitle>
            {HYGIENE_SERIES.map((s) => (
              <TooltipRow color={s.color} key={s.key}>
                {s.label} <b>{(signals[s.key][i] ?? 0).toFixed(1)}%</b>
              </TooltipRow>
            ))}
          </>
        )}
      />
    </ChartSvg>
  );
}

function ContextHistogramChart({
  signals,
  mini,
}: {
  signals: SignalTrends;
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : 1000;
  const h = mini ? MH : 300;
  const pL = mini ? 4 : 44;
  const pR = mini ? 4 : 16;
  const pT = mini ? 6 : 34;
  const pB = mini ? 6 : 28;
  const buckets = signals.ContextHistogram;
  const xScale = scaleLog([8000, 1024000], [pL, w - pR]);
  const maxCount = buckets.reduce((m, b) => Math.max(m, b.Count), 0);
  const yScale = scaleLinear([0, maxCount * 1.08 || 1], [h - pB, pT]);

  return (
    <ChartSvg w={w} h={h}>
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {buckets.map((b) => {
          const x0 = xScale(b.Lo);
          const x1 = xScale(b.Hi);
          const y = yScale(b.Count);
          return (
            <rect
              key={b.Lo}
              x={x0 + 1}
              y={y}
              width={Math.max(1, x1 - x0 - 2)}
              height={h - pB - y}
              fill="var(--viz-1)"
              opacity={mini ? 0.7 : 0.75}
              className="scatter-dot"
            />
          );
        })}
      </ClipRect>
      {!mini && (
        <>
          {[8000, 64000, 512000, 1024000].map((v) => (
            <text
              key={v}
              x={xScale(v)}
              y={h - pB + 17}
              className="axis-tick-text"
              textAnchor="middle"
            >
              {formatTokens(v)}
            </text>
          ))}
          {signals.ContextMarkers.map((m, idx) => {
            const x = xScale(m.Tokens);
            return (
              <g key={m.Kind}>
                <line
                  x1={x}
                  x2={x}
                  y1={pT - 4}
                  y2={h - pB}
                  stroke="var(--subtext)"
                  strokeWidth={1}
                  strokeDasharray="3,3"
                />
                <text
                  x={x}
                  y={pT - 8 - (idx % 2) * 14}
                  className="callout-label"
                  textAnchor="middle"
                  fill="var(--subtext)"
                >
                  {m.Kind} {formatTokens(m.Tokens)}
                </text>
              </g>
            );
          })}
          <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
        </>
      )}
    </ChartSvg>
  );
}

function ContextResetsChart({
  signals,
  n,
  labels,
}: {
  signals: SignalTrends;
  n: number;
  labels: string[];
}) {
  const clipId = useClipId();
  const w = 1000;
  const h = 200;
  const pL = 40;
  const pR = 16;
  const pT = 12;
  const pB = 24;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const maxV = Math.max(...signals.ContextResets, 0) * 1.15 || 1;
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const pts = signals.ContextResets.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={[0, Math.round(maxV / 2), Math.round(maxV)]}
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
        mini={false}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        <path
          d={pathLine(pts)}
          fill="none"
          stroke="var(--viz-4)"
          strokeWidth={2}
        />
        {signals.ContextResets.map((v, i) => (
          <circle
            key={labels[i]}
            cx={xScale(i)}
            cy={yScale(v)}
            r={2}
            fill="var(--viz-4)"
          />
        ))}
      </ClipRect>
      <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
    </ChartSvg>
  );
}

const HTABS = [
  { id: "all", label: "All instruments" },
  { id: "grades", label: "Grades" },
  { id: "outcomes", label: "Outcomes" },
  { id: "hygiene", label: "Hygiene" },
  { id: "context", label: "Context" },
];

export function HealthInstrument({
  insights,
  trends,
  resetKey,
}: {
  insights: Insights;
  trends: Trends;
  resetKey: unknown;
}) {
  const [active, setActive] = useTabState("all", resetKey);
  const signals = trends.Signals;
  const n = trends.BucketStarts.length;
  const gpaLast = signals.GPA[signals.GPA.length - 1] ?? 0;
  const completedLast =
    signals.CompletedRate[signals.CompletedRate.length - 1] ?? 0;
  const noPointerLast =
    signals.HygieneNoCode[signals.HygieneNoCode.length - 1] ?? 0;
  const totalResets = signals.ContextResets.reduce((a, b) => a + b, 0);
  const p50Label =
    insights.Context.Sessions > 0
      ? formatTokens(insights.Context.PeakTokensP50)
      : "";

  return (
    <section className="instrument" id="health" aria-labelledby="health-h">
      <div className="instrument-head">
        <h2 id="health-h">Health</h2>
      </div>
      <div className="panel">
        <TabStrip
          id="health-tabs"
          ariaLabel="Health instruments"
          tabs={HTABS}
          active={active}
          onSelect={setActive}
        />
        <TabPanel stripId="health-tabs" tabId="all" active={active}>
          <div className="grid-2x2">
            <MiniMultipleButton
              onJump={() => setActive("grades")}
              scrollTargetId="health"
            >
              <ChartCaption
                title="Grades"
                value={`GPA ${gpaLast.toFixed(2)}`}
              />
              <GradesChart
                signals={signals}
                n={n}
                labels={trends.Labels}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("outcomes")}
              scrollTargetId="health"
            >
              <ChartCaption
                title="Outcomes"
                value={`${completedLast.toFixed(0)}% completed`}
              />
              <OutcomesChart
                signals={signals}
                n={n}
                labels={trends.Labels}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("hygiene")}
              scrollTargetId="health"
            >
              <ChartCaption
                title="Hygiene"
                value={`${noPointerLast.toFixed(1)}% no pointer`}
              />
              <HygieneChart
                signals={signals}
                n={n}
                labels={trends.Labels}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("context")}
              scrollTargetId="health"
            >
              <ChartCaption title="Context" value={p50Label} />
              <ContextHistogramChart signals={signals} mini />
            </MiniMultipleButton>
          </div>
        </TabPanel>
        <TabPanel stripId="health-tabs" tabId="grades" active={active}>
          <GradesChart
            signals={signals}
            n={n}
            labels={trends.Labels}
            mini={false}
          />
          <Legend
            items={GRADE_ORDER.map((key) => ({
              color: gradeColor(key),
              label: gradeLabel(key),
            }))}
          />
          <InstrumentCaption lead="How session quality graded over time.">
            <code>session_signals.grade</code> re-read per bucket. The settle
            pass already stamps every session; this is the same column drawn
            over time instead of one rolled-up number.
          </InstrumentCaption>
        </TabPanel>
        <TabPanel stripId="health-tabs" tabId="outcomes" active={active}>
          <OutcomesChart
            signals={signals}
            n={n}
            labels={trends.Labels}
            mini={false}
          />
          <InstrumentCaption lead="How sessions ended over time: completed against abandoned.">
            <code>session_signals.outcome</code> per bucket, with raw session
            counts underneath.
          </InstrumentCaption>
        </TabPanel>
        <TabPanel stripId="health-tabs" tabId="hygiene" active={active}>
          <HygieneChart
            signals={signals}
            n={n}
            labels={trends.Labels}
            mini={false}
          />
          <Legend
            items={HYGIENE_SERIES.map((s) => ({
              color: s.color,
              label: s.label,
            }))}
          />
          <InstrumentCaption lead="How well-formed the prompts were over time: short, code-free, or repeated.">
            <code>messages.prompt_facts</code> classified per prompt, rolled up
            per bucket as a share of the bucket's prompts touched by each flag.
          </InstrumentCaption>
        </TabPanel>
        <TabPanel stripId="health-tabs" tabId="context" active={active}>
          <div>
            <ChartCaption title="Peak context per session" value={p50Label} />
            <div className="overflow-x">
              <div style={{ minWidth: 480 }}>
                <ContextHistogramChart signals={signals} mini={false} />
              </div>
            </div>
          </div>
          <div style={{ marginTop: 20 }}>
            <ChartCaption
              title="Weekly context resets"
              value={`${totalResets} total`}
            />
            <div className="overflow-x">
              <div style={{ minWidth: 480 }}>
                <ContextResetsChart
                  signals={signals}
                  n={n}
                  labels={trends.Labels}
                />
              </div>
            </div>
          </div>
          <p className="panel-caption">
            session_signals context peaks and weekly reset counts, from the same
            settle-pass rows as the other Health instruments.
          </p>
          <InstrumentCaption lead="How close sessions ran to the model's context limit.">
            Peak context tokens per session, banded on the absolute token scale,
            so a fleet crowding its context window reads before sessions start
            dropping work.
          </InstrumentCaption>
        </TabPanel>
      </div>
    </section>
  );
}
