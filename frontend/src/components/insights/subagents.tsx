import type { Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import { InstrumentCaption } from "./caption";
import { fanoutColor, fanoutLabel, fmtInt } from "./format";
import { Legend } from "./legend";
import {
  AxisBaseline,
  AxisTicksY,
  BucketAxis,
  ChartSvg,
  ClipRect,
  HoverBucket,
  pathBand,
  pathLine,
  resolveLabelCollisions,
  scaleLinear,
  TooltipRow,
  TooltipTitle,
  useClipId,
} from "./primitives";

const W = 1000;
const H = 380;

function DelegationChart({
  n,
  labels,
  delegateShare,
  costShare,
}: {
  n: number;
  labels: string[];
  delegateShare: number[];
  costShare: number[];
}) {
  const clipId = useClipId();
  const pL = 40;
  const pR = 90;
  const pT = 14;
  const pB = 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, W - pR]);
  const maxV = Math.max(1, Math.max(...delegateShare, ...costShare, 0) * 1.15);
  const yScale = scaleLinear([0, maxV], [H - pB, pT]);
  const delPts = delegateShare.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const costPts = costShare.map(
    (v, i) => [xScale(i), yScale(v)] as [number, number],
  );
  const lastDel = delPts[delPts.length - 1];
  const lastCost = costPts[costPts.length - 1];
  const pendingLabels = [
    lastDel && {
      y: lastDel[1],
      color: "var(--accent)",
      text: `delegate ${(delegateShare[delegateShare.length - 1] ?? 0).toFixed(0)}%`,
    },
    lastCost && {
      y: lastCost[1],
      color: "var(--muted)",
      text: `cost share ${(costShare[costShare.length - 1] ?? 0).toFixed(0)}%`,
    },
  ].filter((v): v is { y: number; color: string; text: string } => Boolean(v));

  return (
    <ChartSvg w={W} h={H}>
      <AxisTicksY
        values={[0, Math.round(maxV / 2), Math.round(maxV)]}
        xLeft={pL}
        xRight={W - pR}
        yScale={yScale}
        fmt={(v) => `${v}%`}
      />
      <BucketAxis
        w={W}
        h={H}
        pB={pB}
        pL={pL}
        pR={pR}
        mini={false}
        n={n}
        labels={labels}
      />
      <ClipRect id={clipId} x={pL} y={pT} w={W - pL - pR} h={H - pT - pB}>
        <path
          d={pathLine(delPts)}
          fill="none"
          stroke="var(--accent)"
          strokeWidth={2.2}
        />
        <path
          d={pathLine(costPts)}
          fill="none"
          stroke="var(--muted)"
          strokeWidth={1.6}
          strokeDasharray="3,3"
        />
      </ClipRect>
      {resolveLabelCollisions(pendingLabels, 14, pT, H - pB).map((lbl) => (
        <text
          key={lbl.text}
          x={W - pR + 6}
          y={lbl.y + 3}
          className="callout-label"
          fill={lbl.color}
        >
          {lbl.text}
        </text>
      ))}
      <AxisBaseline x1={pL} x2={W - pR} y={H - pB} />
      <HoverBucket
        w={W}
        h={H}
        pL={pL}
        pR={pR}
        pT={pT}
        pB={pB}
        n={n}
        xScale={xScale}
        tooltip={(i) => (
          <>
            <TooltipTitle>{labels[i]}</TooltipTitle>
            <TooltipRow color="var(--accent)">
              root sessions delegating{" "}
              <b>{(delegateShare[i] ?? 0).toFixed(1)}%</b>
            </TooltipRow>
            <TooltipRow color="var(--muted)">
              cost via subagents <b>{(costShare[i] ?? 0).toFixed(1)}%</b>
            </TooltipRow>
          </>
        )}
      />
    </ChartSvg>
  );
}

function FanoutChart({ trends }: { trends: Trends }) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const s = trends.Subagents;
  const w = W;
  const h = 300;
  const pL = 40;
  const pR = 16;
  const pT = 14;
  const pB = 26;
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [pL, w - pR]);
  const totals = s.FanoutRows.map((r) =>
    s.FanoutOrder.reduce((sum, k) => sum + (r[k] ?? 0), 0),
  );
  const maxV = Math.max(1, Math.max(...totals, 0) * 1.1);
  const yScale = scaleLinear([0, maxV], [h - pB, pT]);
  let cum = new Array(n).fill(0);
  const bands = s.FanoutOrder.map((key) => {
    const bottom = cum.slice();
    const top = cum.map((c, i) => c + (s.FanoutRows[i]?.[key] ?? 0));
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
          labels={trends.Labels}
        />
        <ClipRect id={clipId} x={pL} y={pT} w={w - pL - pR} h={h - pT - pB}>
          {bands.map((b) => (
            <path
              key={b.key}
              d={pathBand(b.topPts, b.bottomPts)}
              fill={fanoutColor(b.key)}
            />
          ))}
        </ClipRect>
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
              <TooltipTitle>{trends.Labels[i]}</TooltipTitle>
              {s.FanoutOrder.map((key) => (
                <TooltipRow color={fanoutColor(key)} key={key}>
                  {fanoutLabel(key)} <b>{s.FanoutRows[i]?.[key] ?? 0}</b>
                </TooltipRow>
              ))}
            </>
          )}
        />
      </ChartSvg>
      <Legend
        items={s.FanoutOrder.map((key) => ({
          color: fanoutColor(key),
          label: fanoutLabel(key),
        }))}
      />
    </>
  );
}

export function SubagentsInstrument({ trends }: { trends: Trends }) {
  const s = trends.Subagents;
  return (
    <section
      className="instrument"
      id="subagents"
      aria-labelledby="subagents-h"
    >
      <div className="instrument-head">
        <h2 id="subagents-h">Subagents</h2>
      </div>
      <div className="panel">
        <StatStrip>
          <Stat
            label="sessions that delegate"
            value={`${Math.round(s.SessionsThatDelegatePct)}%`}
          />
          <Stat
            label="subagent sessions in window"
            value={fmtInt(s.SubagentSessionsInWindow)}
          />
          <Stat
            label="cost run through subagents"
            value={`${Math.round(s.CostThroughSubagentsPct)}%${s.CostShareIncomplete ? " partial" : ""}`}
          />
          <Stat label="deepest tree (levels)" value={String(s.DeepestTree)} />
        </StatStrip>
        <DelegationChart
          n={trends.BucketStarts.length}
          labels={trends.Labels}
          delegateShare={s.DelegateShare}
          costShare={s.CostShare}
        />
        <Legend
          items={[
            { color: "var(--accent)", label: "Root sessions that delegate" },
            { color: "var(--muted)", label: "Cost share via subagents" },
          ]}
        />
        <div style={{ marginTop: 20 }}>
          <FanoutChart trends={trends} />
        </div>
        <InstrumentCaption lead="How much work runs through delegation: how often the fleet spawns subagents, how wide the fan-out runs, and what share of spend rides on it.">
          <code>parent_session_id</code> and <code>relationship_type</code> ride
          every session row and appear in no other aggregate. The stack below
          shows how wide the fan-out runs, not just how often delegation
          happens.
        </InstrumentCaption>
      </div>
    </section>
  );
}
