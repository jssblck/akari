import { formatCost } from "../../format";
import type { Trends } from "../../types";
import {
  ARCHETYPE_ORDER,
  archetypeColor,
  archetypeLabel,
  fmtDuration,
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

  const xLo = durs.length ? Math.max(1, Math.min(30, Math.min(...durs))) : 30;
  const xHi = durs.length ? Math.max(86400, Math.max(...durs) * 1.05) : 86400;
  const yLo = costs.length
    ? Math.max(0.001, Math.min(0.01, Math.min(...costs)))
    : 0.01;
  const yHi = costs.length ? Math.max(60, Math.max(...costs) * 1.08) : 60;
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
            // biome-ignore lint/a11y/noStaticElementInteractions: mouse-only hover tooltip supplements the visual scatter without making each dense dot a keyboard stop.
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
                      cost <b>{formatCost(p.CostUSD)}</b>
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
    </ChartSvg>
  );
}

export function SessionGalleryInstrument({ trends }: { trends: Trends }) {
  return (
    <section className="instrument" id="gallery" aria-labelledby="gallery-h">
      <div className="instrument-head">
        <h2 id="gallery-h">Session gallery</h2>
      </div>
      <div className="panel">
        <div className="overflow-x">
          <div style={{ minWidth: 480 }}>
            <GalleryChart trends={trends} />
          </div>
        </div>
        <Legend
          items={ARCHETYPE_ORDER.map((key) => ({
            color: archetypeColor(key),
            label: archetypeLabel(key),
          }))}
        />
      </div>
    </section>
  );
}
