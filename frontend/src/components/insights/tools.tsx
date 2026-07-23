import type { Insights, ToolPoint } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import {
  CATEGORY_LABEL,
  categoryColor,
  categoryLabel,
  fmtInt,
  fmtK,
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
import { useChartTooltip } from "./tooltip";
import { ChurnTreemap } from "./treemap";

const W = 1000;
const H = 380;
const MW = 420;
const MH = 210;
const FAIL_PALETTE = [
  "var(--err)",
  "var(--viz-4)",
  "var(--viz-1)",
  "var(--viz-6)",
];

function ReliabilityChart({
  tools,
  mini,
}: {
  tools: ToolPoint[];
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : 1000;
  const h = mini ? MH : 380;
  const pL = mini ? 8 : 46;
  const pR = mini ? 8 : 18;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 32;
  const { show, hide } = useChartTooltip();
  const xScale = scaleLog([10, 100000], [pL, w - pR]);
  const yScale = scaleLinear([0, 12], [h - pB, pT]);
  const maxSessions = tools.reduce((m, t) => Math.max(m, t.Sessions), 0) || 1;
  const rScale = (s: number) =>
    (mini ? 1.4 : 2.6) + Math.sqrt(s / maxSessions) * (mini ? 9 : 20);

  return (
    <ChartSvg w={w} h={h}>
      {!mini && (
        <>
          <AxisTicksY
            values={[0, 2, 4, 6, 8, 10, 12]}
            xLeft={pL}
            xRight={w - pR}
            yScale={yScale}
            fmt={(v) => `${v}%`}
          />
          {[10, 100, 1000, 10000, 100000].map((v) => (
            <g key={v}>
              <line
                x1={xScale(v)}
                x2={xScale(v)}
                y1={pT}
                y2={h - pB}
                className="gridline"
              />
              <text
                x={xScale(v)}
                y={h - pB + 17}
                className="axis-tick-text"
                textAnchor="middle"
              >
                {fmtK(v)}
              </text>
            </g>
          ))}
          <AxisBaseline x1={pL} x2={w - pR} y={h - pB} />
        </>
      )}
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {tools.map((tool) => {
          const err = tool.Calls === 0 ? 0 : (tool.Failures / tool.Calls) * 100;
          const cx = xScale(Math.max(tool.Calls, 10));
          const cy = yScale(Math.min(err, 12));
          const r = rScale(tool.Sessions);
          return (
            // biome-ignore lint/a11y/noStaticElementInteractions: mouse-only hover tooltip on a scatter dot; the same figures are already in the stat tiles and Legend below.
            <circle
              key={tool.Name}
              cx={cx}
              cy={cy}
              r={r}
              fill={categoryColor(tool.Category)}
              opacity={0.75}
              stroke="var(--bg)"
              strokeWidth={1}
              className="scatter-dot"
              onMouseMove={
                mini
                  ? undefined
                  : (e) =>
                      show(
                        e.clientX,
                        e.clientY,
                        <>
                          <TooltipTitle>{tool.Name}</TooltipTitle>
                          <TooltipRow>
                            calls <b>{fmtInt(tool.Calls)}</b>
                          </TooltipRow>
                          <TooltipRow>
                            err <b>{err.toFixed(1)}%</b>
                          </TooltipRow>
                          <TooltipRow>
                            sessions <b>{fmtInt(tool.Sessions)}</b>
                          </TooltipRow>
                        </>,
                      )
              }
              onMouseLeave={mini ? undefined : hide}
            />
          );
        })}
      </ClipRect>
    </ChartSvg>
  );
}

function ToolMixChart({
  n,
  labels,
  order,
  mix,
  mini,
}: {
  n: number;
  labels: string[];
  order: string[];
  mix: Record<string, number>[];
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? 26 : 40;
  const pR = mini ? 8 : 16;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const yScale = scaleLinear([0, 100], [h - pB, pT]);
  let cum = new Array(n).fill(0);
  const bands = order.map((key) => {
    const bottom = cum.slice();
    const top = cum.map((c, i) => c + (mix[i]?.[key] ?? 0));
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
    <ChartSvg w={w} h={h}>
      <AxisTicksY
        values={mini ? [0, 100] : [0, 25, 50, 75, 100]}
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
        mini={mini}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        {bands.map((b) => (
          <path
            key={b.key}
            d={pathBand(b.topPts, b.bottomPts)}
            fill={categoryColor(b.key)}
            opacity={0.82}
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
              <TooltipTitle>{labels[i]}</TooltipTitle>
              {order.map((key) => (
                <TooltipRow color={categoryColor(key)} key={key}>
                  {categoryLabel(key)} <b>{(mix[i]?.[key] ?? 0).toFixed(1)}%</b>
                </TooltipRow>
              ))}
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function FailuresChart({
  n,
  labels,
  fleet,
  worst,
  mini,
}: {
  n: number;
  labels: string[];
  fleet: number[];
  worst: { Name: string; Rate: number[] }[];
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : W;
  const h = mini ? MH : H;
  const pL = mini ? 26 : 40;
  const pR = mini ? 8 : 16;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const allVals = worst.reduce(
    (acc: number[], s) => acc.concat(s.Rate),
    fleet.slice(),
  );
  const maxV = Math.max(5, Math.max(...allVals, 0) * 1.15);
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const series = worst
    .map((s, i) => ({
      rate: s.Rate,
      label: s.Name,
      color: FAIL_PALETTE[i % FAIL_PALETTE.length] ?? "var(--err)",
      width: mini ? 1 : 1.4,
    }))
    .concat([
      {
        rate: fleet,
        label: "fleet",
        color: "var(--text)",
        width: mini ? 1.4 : 2,
      },
    ]);
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
        fmt={(v) => `${v}%`}
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
        {series.map((s) => (
          <path
            key={s.label}
            d={pathLine(s.rate.map((v, i) => [xScale(i), yScale(v)]))}
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
              <TooltipTitle>{labels[i]}</TooltipTitle>
              {series.map((s) => (
                <TooltipRow color={s.color} key={s.label}>
                  {s.label} <b>{(s.rate[i] ?? 0).toFixed(1)}%</b>
                </TooltipRow>
              ))}
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

function ChurnTrendChart({
  n,
  labels,
  reedits,
  files,
  mini,
}: {
  n: number;
  labels: string[];
  reedits: number[];
  files: number[];
  mini: boolean;
}) {
  const clipId = useClipId();
  const w = mini ? MW : W;
  const h = mini ? MH : 260;
  const pL = mini ? 26 : 40;
  const pR = mini ? 8 : 16;
  const pT = mini ? 8 : 14;
  const pB = mini ? 16 : 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const maxV = Math.max(1, Math.max(...reedits, ...files, 0)) * 1.15;
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  const rePts = reedits.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const hotPts = files.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );

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
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
        <path
          d={pathArea(rePts, yScale(0))}
          fill="var(--viz-3)"
          opacity={0.16}
        />
        <path
          d={pathLine(rePts)}
          fill="none"
          stroke="var(--viz-3)"
          strokeWidth={mini ? 1.4 : 2}
        />
        <path
          d={pathLine(hotPts)}
          fill="none"
          stroke="var(--muted)"
          strokeWidth={mini ? 1 : 1.3}
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
              <TooltipTitle>{labels[i]}</TooltipTitle>
              <TooltipRow color="var(--viz-3)">
                re-edits <b>{reedits[i]}</b>
              </TooltipRow>
              <TooltipRow color="var(--muted)">
                hot files <b>{files[i]}</b>
              </TooltipRow>
            </>
          )}
        />
      )}
    </ChartSvg>
  );
}

const TTABS = [
  { id: "all", label: "All instruments" },
  { id: "reliability", label: "Reliability" },
  { id: "mix", label: "Mix" },
  { id: "failures", label: "Failures" },
  { id: "churn", label: "Churn" },
];

// ToolsInstrument is the reliability/mix/failures/churn instrument, factored
// as a standalone component over the whole Insights payload so the project
// page's quality band can embed it (it asks for Tools + Churn via
// store.QualityBandPanels, so this component only ever needs those two plus
// the signal-trend-adjacent Trends fields that ride along for free).
export function ToolsInstrument({
  insights,
  resetKey,
}: {
  insights: Insights;
  resetKey: unknown;
}) {
  const trends = insights.Trends;
  const [active, setActive] = useTabState("all", resetKey);
  if (!trends) return null;
  const n = trends.BucketStarts.length;
  const tools = trends.Tools.Reliability;
  const mixOrder = trends.Tools.MixOrder;
  const lastFleet =
    trends.Tools.FailFleet[trends.Tools.FailFleet.length - 1] ?? 0;
  const lastReEdits =
    trends.Churn.ReEdits[trends.Churn.ReEdits.length - 1] ?? 0;
  const busiestFile = insights.Churn.Files.length
    ? insights.Churn.Files.slice().sort((a, b) => b.Edits - a.Edits)[0]
    : null;

  return (
    <section className="instrument" id="tools" aria-labelledby="tools-h">
      <div className="instrument-head">
        <h2 id="tools-h">Tools</h2>
      </div>
      <div className="panel">
        <TabStrip
          id="tools-tabs"
          ariaLabel="Tools instruments"
          tabs={TTABS}
          active={active}
          onSelect={setActive}
        />
        <TabPanel stripId="tools-tabs" tabId="all" active={active}>
          <div className="grid-2x2">
            <MiniMultipleButton
              onJump={() => setActive("reliability")}
              scrollTargetId="tools"
            >
              <ChartCaption
                title="Reliability"
                value={`${tools.length} tools`}
              />
              <ReliabilityChart tools={tools} mini />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("mix")}
              scrollTargetId="tools"
            >
              <ChartCaption
                title="Mix"
                value={
                  mixOrder.length
                    ? `${categoryLabel(mixOrder[0] ?? "")} dominant`
                    : ""
                }
              />
              <ToolMixChart
                n={n}
                labels={trends.Labels}
                order={mixOrder}
                mix={trends.Tools.Mix}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("failures")}
              scrollTargetId="tools"
            >
              <ChartCaption
                title="Failures"
                value={`${lastFleet.toFixed(1)}% fleet`}
              />
              <FailuresChart
                n={n}
                labels={trends.Labels}
                fleet={trends.Tools.FailFleet}
                worst={trends.Tools.FailWorst}
                mini
              />
            </MiniMultipleButton>
            <MiniMultipleButton
              onJump={() => setActive("churn")}
              scrollTargetId="tools"
            >
              <ChartCaption
                title="Churn"
                value={`${fmtInt(lastReEdits)} re-edits/wk`}
              />
              <ChurnTrendChart
                n={n}
                labels={trends.Labels}
                reedits={trends.Churn.ReEdits}
                files={trends.Churn.Files}
                mini
              />
            </MiniMultipleButton>
          </div>
        </TabPanel>
        <TabPanel stripId="tools-tabs" tabId="reliability" active={active}>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ReliabilityChart tools={tools} mini={false} />
            </div>
          </div>
          <Legend
            items={Object.keys(CATEGORY_LABEL).map((cat) => ({
              color: categoryColor(cat),
              label: categoryLabel(cat),
            }))}
          />
        </TabPanel>
        <TabPanel stripId="tools-tabs" tabId="mix" active={active}>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ToolMixChart
                n={n}
                labels={trends.Labels}
                order={mixOrder}
                mix={trends.Tools.Mix}
                mini={false}
              />
            </div>
          </div>
          <Legend
            items={mixOrder.map((cat) => ({
              color: categoryColor(cat),
              label: categoryLabel(cat),
            }))}
          />
        </TabPanel>
        <TabPanel stripId="tools-tabs" tabId="failures" active={active}>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <FailuresChart
                n={n}
                labels={trends.Labels}
                fleet={trends.Tools.FailFleet}
                worst={trends.Tools.FailWorst}
                mini={false}
              />
            </div>
          </div>
        </TabPanel>
        <TabPanel stripId="tools-tabs" tabId="churn" active={active}>
          <StatStrip>
            <Stat
              label="total re-edits"
              value={fmtInt(trends.Churn.TotalReEdits)}
            />
            <Stat
              label="hot files"
              value={fmtInt(trends.Churn.TotalHotFiles)}
            />
            {busiestFile && (
              <Stat
                label="busiest file"
                value={busiestFile.Path.split("/").pop() ?? busiestFile.Path}
                note={`${busiestFile.Edits} edits`}
              />
            )}
          </StatStrip>
          <div className="overflow-x">
            <div style={{ minWidth: 480 }}>
              <ChurnTrendChart
                n={n}
                labels={trends.Labels}
                reedits={trends.Churn.ReEdits}
                files={trends.Churn.Files}
                mini={false}
              />
            </div>
          </div>
          <ChurnTreemap trends={trends} />
        </TabPanel>
      </div>
    </section>
  );
}
