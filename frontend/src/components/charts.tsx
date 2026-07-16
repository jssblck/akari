import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import type { LabeledCount } from "../types";

const chartTheme = {
  grid: "#302e35",
  axis: "#9a94ad",
  lilac: "#c6a8f2",
  teal: "#88cfce",
  peach: "#f0bf92",
};

export function DistributionChart({
  data,
  label,
}: {
  data: LabeledCount[];
  label: string;
}) {
  const normalized = data.map((item) => ({
    name: item.Key || "unscored",
    count: item.Count,
  }));
  return (
    <div className="chart distribution" role="img" aria-label={label}>
      <ResponsiveContainer width="100%" height="100%">
        <BarChart
          data={normalized}
          margin={{ top: 8, right: 8, bottom: 0, left: -16 }}
        >
          <CartesianGrid stroke={chartTheme.grid} vertical={false} />
          <XAxis
            dataKey="name"
            tick={{ fill: chartTheme.axis, fontSize: 11 }}
            tickLine={false}
            axisLine={false}
          />
          <YAxis
            allowDecimals={false}
            tick={{ fill: chartTheme.axis, fontSize: 11 }}
            tickLine={false}
            axisLine={false}
          />
          <Tooltip
            cursor={{ fill: "#242228" }}
            contentStyle={{
              background: "#242228",
              border: "1px solid #57535f",
              borderRadius: 4,
            }}
          />
          <Bar
            dataKey="count"
            fill={chartTheme.teal}
            radius={[3, 3, 0, 0]}
            isAnimationActive={false}
          />
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

export function ActivityBars({ values }: { values: number[] | undefined }) {
  const observed =
    values?.map((value) => (Number.isFinite(value) && value > 0 ? value : 0)) ??
    [];
  const buckets =
    observed.length > 0 ? observed : Array.from({ length: 30 }, () => 0);
  const peak = Math.max(...buckets, 0);
  const width = 120;
  const height = 24;
  const gap = 1;
  const barWidth =
    (width - gap * Math.max(buckets.length - 1, 0)) / buckets.length;
  return (
    <svg
      className="activity-bars"
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label="Daily project tokens over the last 30 days"
    >
      {buckets.map((value, index) => {
        const active = value > 0 && peak > 0;
        const barHeight = active
          ? Math.max(2, Math.sqrt(value / peak) * (height - 2))
          : 1;
        return (
          <rect
            // biome-ignore lint/suspicious/noArrayIndexKey: each index is a stable calendar-day bucket in the fixed 30-day series.
            key={index}
            x={index * (barWidth + gap)}
            y={height - barHeight}
            width={Math.max(barWidth, 0.5)}
            height={barHeight}
            rx={0.6}
            fill={chartTheme.lilac}
            opacity={active ? 0.88 : 0.14}
          />
        );
      })}
    </svg>
  );
}
