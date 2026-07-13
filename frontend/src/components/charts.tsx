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

export function Sparkline({ values }: { values: number[] | undefined }) {
  if (!values || values.length === 0) return <span className="muted">-</span>;
  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${Math.max(values.length - 1, 1)} 20`}
      preserveAspectRatio="none"
      aria-hidden="true"
    >
      <polyline
        fill="none"
        stroke={chartTheme.lilac}
        strokeWidth="1.4"
        vectorEffect="non-scaling-stroke"
        points={values
          .map((value, index) => `${index},${20 - value * 20}`)
          .join(" ")}
      />
    </svg>
  );
}
