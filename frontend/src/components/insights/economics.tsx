import { formatCost } from "../../format";
import type { Economics, Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import { fmtInt, fmtK } from "./format";
import { Legend } from "./legend";
import {
  AxisBaseline,
  AxisTicksY,
  BucketAxis,
  ChartSvg,
  ClipRect,
  HoverBucket,
  pathArea,
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

const W = 1000;
const H = 380;
const mW = 480;
const mH = 220;

function CostQualityChart({
  n,
  labels,
  economics,
  mini,
}: {
  n: number;
  labels: string[];
  economics: Economics;
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? mW : W;
  const h = mini ? mH : H;
  const pL = mini ? 30 : 46;
  const pR = mini ? 8 : 16;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const totals = economics.CostCompleted.map(
    (v, i) =>
      v + (economics.CostAbandoned[i] ?? 0) + (economics.CostOther[i] ?? 0),
  );
  const maxV = (Math.max(...totals, 0) || 1) * 1.1;
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const bw = (w - pL - pR) / n - (mini ? 1 : 2);

  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={
          mini
            ? [0, Math.round(maxV)]
            : [0, Math.round(maxV / 2), Math.round(maxV)]
        }
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => `$${fmtK(v)}`}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={mini}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {economics.CostCompleted.map((completed, i) => {
          const abandoned = economics.CostAbandoned[i] ?? 0;
          const other = economics.CostOther[i] ?? 0;
          const x = xScale(i) - bw / 2;
          const yComp = yScale(completed);
          const yAband = yScale(completed + abandoned);
          const yTot = yScale(completed + abandoned + other);
          return (
            <g key={labels[i]}>
              <rect
                x={x}
                y={yComp}
                width={bw}
                height={h - pB - yComp}
                fill="var(--ok)"
                opacity={0.78}
              />
              <rect
                x={x}
                y={yAband}
                width={bw}
                height={yComp - yAband}
                fill="var(--warn)"
                opacity={0.82}
              />
              {other > 0 && (
                <rect
                  x={x}
                  y={yTot}
                  width={bw}
                  height={yAband - yTot}
                  fill="var(--muted)"
                  opacity={0.5}
                />
              )}
            </g>
          );
        })}
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
              <TooltipTitle>{labels[i]}</TooltipTitle>
              <TooltipRow color="var(--ok)">
                completed <b>${(economics.CostCompleted[i] ?? 0).toFixed(0)}</b>
              </TooltipRow>
              <TooltipRow color="var(--warn)">
                abandoned <b>${(economics.CostAbandoned[i] ?? 0).toFixed(0)}</b>
              </TooltipRow>
              {(economics.CostOther[i] ?? 0) > 0 && (
                <TooltipRow color="var(--muted)">
                  other <b>${(economics.CostOther[i] ?? 0).toFixed(0)}</b>
                </TooltipRow>
              )}
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function CacheChart({
  n,
  labels,
  economics,
  mini,
}: {
  n: number;
  labels: string[];
  economics: Economics;
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? mW : W;
  const h = mini ? mH : H;
  const pL = mini ? 30 : 46;
  const pR = mini ? 8 : 60;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const maxSavings = (Math.max(...economics.CacheSavings, 0) || 1) * 1.15;
  const yScale = scaleLinear([0, maxSavings], [h - pB, pT]);
  const yScaleHit = scaleLinear([80, 92], [h - pB, pT]);
  const pts = economics.CacheSavings.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const measuredPts = economics.CacheHitRate.map(
    (v, i) => [xScale(i), yScaleHit(v)] as [number, number],
  ).filter((_, i) => economics.CacheMeasured[i]);
  return (
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={
          mini
            ? [0, Math.round(maxSavings)]
            : [0, Math.round(maxSavings / 2), Math.round(maxSavings)]
        }
        xLeft={pL}
        xRight={w - pR}
        yScale={yScale}
        fmt={(v) => `$${v}`}
      />
      <BucketAxis
        w={w}
        h={h}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={mini}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        <path d={pathArea(pts, yScale(0))} fill="var(--viz-2)" opacity={0.22} />
        <path
          d={pathLine(pts)}
          fill="none"
          stroke="var(--viz-2)"
          strokeWidth={mini ? 1.4 : 2}
        />
        {measuredPts.length > 0 && (
          <path
            d={pathLine(measuredPts)}
            fill="none"
            stroke="var(--viz-7)"
            strokeWidth={mini ? 1.2 : 1.6}
            strokeDasharray="3,3"
          />
        )}
      </ClipRect>
      {!mini && (
        <>
          {[80, 85, 90].map((v) => (
            <text
              key={v}
              x={w - pR + 6}
              y={yScaleHit(v) + 3}
              className="axis-tick-text"
              textAnchor="start"
            >
              {v}%
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
                <TooltipRow color="var(--viz-2)">
                  savings <b>${(economics.CacheSavings[i] ?? 0).toFixed(0)}</b>
                </TooltipRow>
                <TooltipRow color="var(--viz-7)">
                  hit rate{" "}
                  <b>
                    {economics.CacheMeasured[i]
                      ? `${(economics.CacheHitRate[i] ?? 0).toFixed(1)}%`
                      : "n/a"}
                  </b>
                </TooltipRow>
              </>
            )}
          />
        </>
      )}
    </ChartSvg>
  );
}

const ETABS = [
  { id: "all", label: "All instruments" },
  { id: "costquality", label: "Cost of quality" },
  { id: "cache", label: "Cache" },
];

export function EconomicsInstrument({
  trends,
  resetKey,
}: {
  trends: Trends;
  resetKey: unknown;
}) {
  const [active, setActive] = useTabState("all", resetKey);
  const n = trends.BucketStarts.length;
  const e = trends.Economics;
  const gallery = trends.Gallery;
  const spendMark = e.CostIncomplete ? "+" : "";
  const perDollar = e.TotalSpend > 0 ? e.TotalCacheSavings / e.TotalSpend : 0;
  const perDollarMark =
    e.CacheSavingsIncomplete || e.CostIncomplete ? " partial" : "";

  return (
    <section
      className="instrument"
      id="economics"
      aria-labelledby="economics-h"
    >
      <div className="instrument-head">
        <h2 id="economics-h">Economics</h2>
      </div>
      <div className="panel">
        <TabStrip
          id="economics-tabs"
          ariaLabel="Economics instruments"
          tabs={ETABS}
          active={active}
          onSelect={setActive}
        />
        <TabPanel stripId="economics-tabs" tabId="all" active={active}>
          <div className="grid-2">
            <MiniMultipleButton
              onJump={() => setActive("costquality")}
              scrollTargetId="economics"
            >
              <ChartCaption
                title="Cost of quality"
                value={`${e.AbandonedSharePct.toFixed(0)}% abandoned $`}
              />
              <CostQualityChart
                n={n}
                labels={trends.Labels}
                economics={e}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("cache")}
              scrollTargetId="economics"
            >
              <ChartCaption
                title="Cache"
                value={`$${fmtInt(e.TotalCacheSavings)} saved`}
              />
              <CacheChart n={n} labels={trends.Labels} economics={e} mini />
            </MiniMultipleButton>
          </div>
        </TabPanel>
        <TabPanel stripId="economics-tabs" tabId="costquality" active={active}>
          <StatStrip>
            <Stat
              label="total spend"
              value={`$${fmtInt(e.TotalSpend)}${spendMark}`}
            />
            <Stat
              label="sunk"
              value={`$${fmtInt(e.TotalAbandoned)}${spendMark}`}
            />
            <Stat
              label="median $ per completed session"
              value={formatCost(
                gallery.MedianCompletedCostUSD,
                gallery.CostIncomplete,
              )}
            />
          </StatStrip>
          <CostQualityChart
            n={n}
            labels={trends.Labels}
            economics={e}
            mini={false}
          />
          <Legend
            items={[
              { color: "var(--ok)", label: "Completed sessions" },
              { color: "var(--warn)", label: "Abandoned sessions" },
              { color: "var(--muted)", label: "Other outcomes" },
            ]}
          />
        </TabPanel>
        <TabPanel stripId="economics-tabs" tabId="cache" active={active}>
          <StatStrip>
            <Stat
              label="savings total"
              value={`$${fmtInt(e.TotalCacheSavings)}${e.CacheSavingsIncomplete ? " partial" : ""}`}
            />
            <Stat
              label="hit rate"
              value={`${e.CacheHitRateLatest.toFixed(0)}%`}
            />
            <Stat
              label="savings per $1 spent"
              value={`$${perDollar.toFixed(2)}${perDollarMark}`}
            />
          </StatStrip>
          <CacheChart n={n} labels={trends.Labels} economics={e} mini={false} />
        </TabPanel>
      </div>
    </section>
  );
}
