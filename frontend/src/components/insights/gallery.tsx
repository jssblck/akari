import { formatCost } from "../../format";
import type { Trends } from "../../types";
import { Stat, StatStrip } from "../stat-strip";
import { InstrumentCaption } from "./caption";
import {
  ARCHETYPE_ORDER,
  archetypeColor,
  archetypeLabel,
  fmtCostShort,
  fmtDuration,
  fmtDurationShort,
} from "./format";
import { Legend } from "./legend";
import {
  AxisBaseline,
  AxisTicksY,
  ChartSvg,
  ClipRect,
  scaleLog,
  TooltipRow,
  TooltipTitle,
  useClipId,
} from "./primitives";
import { useChartTooltip } from "./tooltip";

const W = 1000;
const H = 380;
const PL = 46;
const PR = 24;
const PT = 16;
const PB = 30;

const X_GRID = [30, 300, 3600, 43200, 86400];
const Y_TICKS = [0.01, 0.1, 1, 10, 60];

function GalleryChart({ trends }: { trends: Trends }) {
  const clipId = useClipId();
  const gallery = trends.Gallery;
  const { show, hide } = useChartTooltip();

  const durs = gallery.Rows.map((r) => r.DurationS);
  const costs = gallery.Rows.map((r) => r.CostUSD);
  const annotated = gallery.Total >= 8;
  const annotations: {
    durationS: number;
    costUSD: number;
    label: string;
    corner: "top-right" | "bottom-left";
  }[] = [];
  if (annotated) {
    annotations.push({
      durationS: gallery.PriciestDurationS,
      costUSD: gallery.PriciestCostUSD,
      label: fmtCostShort(gallery.PriciestCostUSD),
      corner: "top-right",
    });
    if (gallery.LongestDurationS !== gallery.PriciestDurationS) {
      annotations.push({
        durationS: gallery.LongestDurationS,
        costUSD: gallery.LongestCostUSD,
        label: fmtDurationShort(gallery.LongestDurationS),
        corner: "bottom-left",
      });
    }
  }
  const durFit = durs.concat(annotations.map((a) => a.durationS));
  const costFit = costs.concat(annotations.map((a) => a.costUSD));

  const xLo = durFit.length
    ? Math.max(1, Math.min(30, Math.min(...durFit)))
    : 30;
  const xHi = durFit.length
    ? Math.max(86400, Math.max(...durFit) * 1.05)
    : 86400;
  const yLo = costFit.length
    ? Math.max(0.001, Math.min(0.01, Math.min(...costFit)))
    : 0.01;
  const yHi = costFit.length ? Math.max(60, Math.max(...costFit) * 1.08) : 60;
  const xScale = scaleLog([xLo, xHi], [PL, W - PR]);
  const yScale = scaleLog([yLo, yHi], [H - PB, PT]);

  return (
    <ChartSvg w={W} h={H}>
      <AxisTicksY
        values={Y_TICKS}
        xLeft={PL}
        xRight={W - PR}
        yScale={yScale}
        fmt={(v) => (v < 1 ? `$${v.toFixed(2)}` : `$${v}`)}
      />
      {X_GRID.map((v) => (
        <g key={v}>
          <line
            x1={xScale(v)}
            x2={xScale(v)}
            y1={PT}
            y2={H - PB}
            className="gridline"
          />
          <text
            x={xScale(v)}
            y={H - PB + 15}
            className="axis-tick-text"
            textAnchor="middle"
          >
            {fmtDuration(v)}
          </text>
        </g>
      ))}
      <AxisBaseline x1={PL} x2={W - PR} y={H - PB} />
      <ClipRect id={clipId} x={PL} y={PT} w={W - PL - PR} h={H - PT - PB}>
        {gallery.Rows.map((p, i) => {
          const cx = Math.max(PL, Math.min(W - PR, xScale(p.DurationS)));
          const cy = Math.max(PT, Math.min(H - PB, yScale(p.CostUSD)));
          return (
            // biome-ignore lint/a11y/noStaticElementInteractions: mouse-only hover tooltip on a scatter dot; duration/cost/grade/outcome are already summarized in the stat tiles above.
            <circle
              // biome-ignore lint/suspicious/noArrayIndexKey: rows carry no stable id
              key={i}
              cx={cx}
              cy={cy}
              r={3.4}
              fill={archetypeColor(p.Archetype)}
              opacity={0.7}
              className="scatter-dot"
              onMouseMove={(e) =>
                show(
                  e.clientX,
                  e.clientY,
                  <>
                    <TooltipTitle>{archetypeLabel(p.Archetype)}</TooltipTitle>
                    <TooltipRow>
                      duration <b>{fmtDuration(p.DurationS)}</b>
                    </TooltipRow>
                    <TooltipRow>
                      cost <b>{formatCost(p.CostUSD, p.CostIncomplete)}</b>
                    </TooltipRow>
                    <TooltipRow>
                      grade <b>{p.Grade || "unscored"}</b>
                    </TooltipRow>
                    <TooltipRow>
                      outcome <b>{p.Outcome}</b>
                    </TooltipRow>
                  </>,
                )
              }
              onMouseLeave={hide}
            />
          );
        })}
      </ClipRect>
      {annotations.map((a) => {
        const x = xScale(a.durationS);
        const y = yScale(a.costUSD);
        const dx = a.corner === "top-right" ? 70 : -70;
        const dy = a.corner === "top-right" ? -34 : 34;
        const lx = x + dx;
        const ly = y + dy;
        return (
          <g key={a.corner}>
            <line
              x1={x}
              y1={y}
              x2={lx}
              y2={ly}
              stroke="var(--subtext)"
              strokeWidth={1}
            />
            <text
              x={a.corner === "top-right" ? lx - 4 : lx + 4}
              y={ly}
              className="callout-label"
              textAnchor={a.corner === "top-right" ? "end" : "start"}
            >
              {a.label}
            </text>
          </g>
        );
      })}
    </ChartSvg>
  );
}

export function SessionGalleryInstrument({ trends }: { trends: Trends }) {
  const gallery = trends.Gallery;
  return (
    <section className="instrument" id="gallery" aria-labelledby="gallery-h">
      <div className="instrument-head">
        <h2 id="gallery-h">Session gallery</h2>
      </div>
      <div className="panel">
        <StatStrip>
          <Stat
            label="median duration"
            value={fmtDuration(gallery.MedianDurationS)}
          />
          <Stat
            label="median cost"
            value={formatCost(gallery.MedianCostUSD, gallery.CostIncomplete)}
          />
          <Stat
            label="priciest session"
            value={formatCost(gallery.PriciestCostUSD, gallery.CostIncomplete)}
            note={fmtDuration(gallery.PriciestDurationS)}
          />
        </StatStrip>
        <GalleryChart trends={trends} />
        {gallery.Rows.length < gallery.Total && (
          <p className="panel-caption" style={{ marginTop: 6 }}>
            Scatter shows the {gallery.Rows.length.toLocaleString()} most recent
            of {gallery.Total.toLocaleString()} sessions; the figures cover all{" "}
            {gallery.Total.toLocaleString()}.
          </p>
        )}
        <Legend
          items={ARCHETYPE_ORDER.map((key) => ({
            color: archetypeColor(key),
            label: archetypeLabel(key),
          }))}
        />
        <InstrumentCaption lead="Every session as one dot, so the long or expensive outliers stop hiding inside the averages.">
          Every fully spanned session is one dot, placed by how long it ran and
          what it cost, coloured by archetype.
        </InstrumentCaption>
      </div>
    </section>
  );
}
