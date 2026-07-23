import { formatCount } from "../../format";
import type { Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
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
  scaleLinear,
  TooltipRow,
  TooltipTitle,
  useClipId,
} from "./primitives";

const W = 1000;
const H = 380;
const PL = 34;
const PR = 24;
const PT = 14;
const PB = 24;

// modelStyle assigns each model its ordinal viz hue (skipping "other", which
// is always var(--muted) and never consumes a ramp slot) and its
// prettified label, in the order the server already ranked them. Returns
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
  // Stripping a vendor prefix can land two different models on one label, so any
  // shortened form claimed by more than one identifier goes back to the full id.
  // Two models rendering as the same chip would read as one model.
  const claims: Record<string, number> = {};
  for (const label of Object.values(labels)) {
    claims[label] = (claims[label] ?? 0) + 1;
  }
  for (const [model, label] of Object.entries(labels)) {
    if ((claims[label] ?? 0) > 1) labels[model] = model;
  }
  return {
    colorOf: (model: string) => colors[model] ?? "var(--muted)",
    labelOf: (model: string) => labels[model] ?? prettyModel(model),
  };
}

// axisLabelsWithYear appends the calendar year to every label when the
// window spans more than one year. The Year and All ranges bucket weekly
// with no year in the label ("Jan 2"), so the axis's three ticks (first,
// middle, last) can otherwise land on the same month name a year apart and
// read as ambiguous rather than as a long window.
function axisLabelsWithYear(
  labels: string[],
  bucketStarts: string[],
): string[] {
  const first = bucketStarts[0];
  const last = bucketStarts[bucketStarts.length - 1];
  if (!first || !last) return labels;
  // Bucket starts are UTC instants and the server's own "Jan 2" labels are
  // formatted from the same instants, so the year has to come from the UTC
  // calendar too: reading it in the viewer's local time zone would round a
  // midnight UTC bucket into the wrong year for anyone west of Greenwich.
  const firstYear = new Date(first).getUTCFullYear();
  const lastYear = new Date(last).getUTCFullYear();
  if (firstYear === lastYear) return labels;
  return labels.map((label, i) => {
    const start = bucketStarts[i];
    return start ? `${label}, ${new Date(start).getUTCFullYear()}` : label;
  });
}

function FleetMixChart({ trends }: { trends: Trends }) {
  const clipId = useClipId();
  const n = trends.BucketStarts.length;
  const models = trends.FleetMix.Models;
  const { colorOf, labelOf } = modelStyle(models);
  const axisLabels = axisLabelsWithYear(trends.Labels, trends.BucketStarts);
  const xScale = scaleLinear([0, Math.max(n - 1, 1)], [PL, W - PR]);
  const yScale = scaleLinear([0, 100], [H - PB, PT]);

  // Stack bottom-up in the server's Models order (tokens desc, "other" last).
  let cum = new Array(n).fill(0);
  const bands: {
    key: string;
    bottomPts: [number, number][];
    topPts: [number, number][];
  }[] = [];
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
    cum = top;
  }

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
        labels={axisLabels}
      />
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
            <TooltipTitle>{axisLabels[i]}</TooltipTitle>
            {models.map((m) => (
              <TooltipRow
                color={colorOf(m.Model)}
                key={m.Model}
                title={m.Model}
              >
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
              value={
                <span title={busiest.Model}>{labelOf(busiest.Model)}</span>
              }
            />
          )}
          {showArrival && (
            <Stat
              label="newest arrival"
              value={
                <span title={trends.FleetMix.NewestModel}>
                  {prettyModel(trends.FleetMix.NewestModel)}
                </span>
              }
            />
          )}
        </StatStrip>
        <div className="overflow-x">
          <div style={{ minWidth: 480 }}>
            <FleetMixChart trends={trends} />
          </div>
        </div>
        <Legend
          items={models.map((m) => ({
            color: colorOf(m.Model),
            label: labelOf(m.Model),
            title: m.Model,
          }))}
        />
      </div>
    </section>
  );
}
