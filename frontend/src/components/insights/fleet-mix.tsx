import { formatCount } from "../../format";
import type { Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import { InstrumentCaption } from "./caption";
import { pickVizVar, prettyModel } from "./format";
import { Legend } from "./legend";
import {
  AxisBaseline,
  AxisTicksY,
  BucketAxis,
  ChartSvg,
  ClipRect,
  HoverBucket,
  pathBand,
  resolveLabelCollisions,
  scaleLinear,
  TooltipRow,
  TooltipTitle,
  useClipId,
} from "./primitives";

const W = 1000;
const H = 380;
const PL = 34;
const PR = 130;
const PT = 14;
const PB = 24;

// modelStyle assigns each model its ordinal viz hue (skipping "other", which
// is always var(--muted) and never consumes a ramp slot) and its
// prettified label, matching the server's fleetMixData exactly. Returns
// lookup functions rather than raw maps so every call site gets a plain
// string back instead of repeating a fallback at each use.
function modelStyle(models: Trends["FleetMix"]["Models"]) {
  const colors: Record<string, string> = {};
  const labels: Record<string, string> = {};
  let slot = 0;
  for (const m of models) {
    if (m.Model === "other") {
      colors[m.Model] = "var(--muted)";
    } else {
      colors[m.Model] = pickVizVar(slot);
      slot++;
    }
    labels[m.Model] = prettyModel(m.Model);
  }
  return {
    colorOf: (model: string) => colors[model] ?? "var(--muted)",
    labelOf: (model: string) => labels[model] ?? prettyModel(model),
  };
}

function FleetMixChart({ trends }: { trends: Trends }) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const models = trends.FleetMix.Models;
  const { colorOf, labelOf } = modelStyle(models);
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [PL, W - PR]);
  const yScale = scaleLinear([0, 100], [H - PB, PT]);

  // Stack bottom-up in the server's Models order (tokens desc, "other"
  // last), tracking each model's cumulative [bottom, top] band at every
  // bucket so the trailing-bucket right-edge label can center on its own
  // band rather than recomputing the stack from scratch.
  let cum = new Array(n).fill(0);
  const bands: {
    key: string;
    bottomPts: [number, number][];
    topPts: [number, number][];
  }[] = [];
  const lastBand: Record<string, [number, number]> = {};
  for (const m of models) {
    const bottom = cum.slice();
    const top = cum.map((c, i) => c + (m.Share[i] ?? 0));
    bands.push({
      key: m.Model,
      bottomPts: bottom.map(
        (v, i) => [xScale(i), yScale(v)] as [number, number],
      ),
      topPts: top.map((v, i) => [xScale(i), yScale(v)] as [number, number]),
    });
    lastBand[m.Model] = [bottom[n - 1] ?? 0, top[n - 1] ?? 0];
    cum = top;
  }

  const arrival = trends.FleetMix.NewestFirst;
  const showArrival = arrival > 0 && arrival < n;

  const pendingLabels = models
    .map((m) => {
      const [lo, hi] = lastBand[m.Model] ?? [0, 0];
      const share = hi - lo;
      if (share < 3) return null;
      return {
        y: yScale((lo + hi) / 2),
        color: colorOf(m.Model),
        text: `${labelOf(m.Model)} ${share.toFixed(0)}%`,
      };
    })
    .filter((v): v is { y: number; color: string; text: string } => v !== null);

  return (
    <ChartSvg w={W} h={H}>
      <AxisTicksY
        values={[0, 25, 50, 75, 100]}
        xLeft={PL}
        xRight={W - PR}
        yScale={yScale}
        fmt={(v) => `${v}%`}
      />
      <BucketAxis
        w={W}
        h={H}
        pB={PB}
        pL={PL}
        pR={PR}
        mini={false}
        n={n}
        labels={trends.Labels}
      />
      {showArrival && (
        <line
          x1={xScale(arrival)}
          x2={xScale(arrival)}
          y1={PT}
          y2={H - PB}
          stroke="var(--faint)"
          strokeWidth={1}
          strokeDasharray="2,3"
        />
      )}
      <ClipRect id={clipId} x={PL} y={PT} w={W - PL - PR} h={H - PT - PB}>
        {bands.map((b) => (
          <path
            key={b.key}
            d={pathBand(b.topPts, b.bottomPts)}
            fill={colorOf(b.key)}
            opacity={0.85}
          />
        ))}
      </ClipRect>
      <AxisBaseline x1={PL} x2={W - PR} y={H - PB} />
      {resolveLabelCollisions(pendingLabels, 14, PT, H - PB).map((lbl) => (
        <text
          key={lbl.text}
          x={W - PR + 8}
          y={lbl.y + 3}
          className="axis-tick-text"
          fill={lbl.color}
          fontFamily="Geist Mono, monospace"
          fontSize={11}
        >
          {lbl.text}
        </text>
      ))}
      <HoverBucket
        w={W}
        h={H}
        pL={PL}
        pR={PR}
        pT={PT}
        pB={PB}
        n={n}
        xScale={xScale}
        tooltip={(i) => (
          <>
            <TooltipTitle>{trends.Labels[i]}</TooltipTitle>
            {models.map((m) => (
              <TooltipRow color={colorOf(m.Model)} key={m.Model}>
                {labelOf(m.Model)} <b>{(m.Share[i] ?? 0).toFixed(1)}%</b>
              </TooltipRow>
            ))}
          </>
        )}
      />
    </ChartSvg>
  );
}

export function FleetMixInstrument({ trends }: { trends: Trends }) {
  const models = trends.FleetMix.Models;
  const { colorOf, labelOf } = modelStyle(models);
  const nonOther = models.filter((m) => m.Model !== "other");
  const busiest = nonOther.length
    ? nonOther.reduce((a, b) => (b.WindowShare > a.WindowShare ? b : a))
    : null;
  const arrival = trends.FleetMix.NewestFirst;
  const showArrival =
    trends.FleetMix.NewestModel !== "" &&
    arrival > 0 &&
    arrival < trends.Labels.length;

  return (
    <section className="instrument" id="fleetmix" aria-labelledby="fleetmix-h">
      <div className="instrument-head">
        <h2 id="fleetmix-h">Fleet mix</h2>
      </div>
      <div className="panel">
        <StatStrip>
          <Stat label="models in window" value={formatCount(models.length)} />
          {busiest && (
            <Stat
              label="busiest model"
              value={labelOf(busiest.Model)}
              note={`${busiest.WindowShare.toFixed(0)}% of window tokens`}
            />
          )}
          {showArrival && (
            <Stat
              label="newest arrival"
              value={prettyModel(trends.FleetMix.NewestModel)}
              note={`first seen ${trends.Labels[arrival]}`}
            />
          )}
        </StatStrip>
        <FleetMixChart trends={trends} />
        <Legend
          items={models.map((m) => ({
            color: colorOf(m.Model),
            label: labelOf(m.Model),
          }))}
        />
        <InstrumentCaption lead="Which models did the work, and how the mix shifted across the window.">
          <code>usage_events.model</code> token share per bucket; a migration
          shows up here as one band growing while another shrinks, without
          reading release notes.
        </InstrumentCaption>
      </div>
    </section>
  );
}
