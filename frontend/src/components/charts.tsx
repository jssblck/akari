import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import { formatCost, formatCount } from "../format";
import type { Analytics, LabeledCount } from "../types";

const chartTheme = {
  grid: "#302e35",
  axis: "#9a94ad",
  lilac: "#c6a8f2",
  teal: "#88cfce",
  peach: "#f0bf92",
};

export function UsageChart({
  analytics,
  metric = "cost",
}: {
  analytics: Analytics;
  metric?: "cost" | "tokens";
}) {
  const data = (analytics.Series ?? []).map((point) => ({
    day: point.Day.slice(0, 10),
    cost: point.CostUSD,
    tokens: point.Input + point.Output + point.CacheRead + point.CacheWrite,
  }));
  if (data.length === 0)
    return <div className="chart-empty">No dated usage in this window.</div>;
  const key = metric === "cost" ? "cost" : "tokens";
  return (
    <div className="chart" role="img" aria-label={`${metric} over time`}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart
          data={data}
          margin={{ top: 12, right: 12, bottom: 0, left: 4 }}
        >
          <defs>
            <linearGradient id={`usage-${metric}`} x1="0" x2="0" y1="0" y2="1">
              <stop
                offset="0%"
                stopColor={chartTheme.lilac}
                stopOpacity={0.34}
              />
              <stop
                offset="100%"
                stopColor={chartTheme.lilac}
                stopOpacity={0}
              />
            </linearGradient>
          </defs>
          <CartesianGrid stroke={chartTheme.grid} vertical={false} />
          <XAxis
            dataKey="day"
            tick={{ fill: chartTheme.axis, fontSize: 11 }}
            tickLine={false}
            axisLine={false}
            minTickGap={36}
          />
          <YAxis
            tick={{ fill: chartTheme.axis, fontSize: 11 }}
            tickLine={false}
            axisLine={false}
            width={48}
            tickFormatter={(value: number) =>
              metric === "cost" ? `$${formatCount(value)}` : formatCount(value)
            }
          />
          <Tooltip
            contentStyle={{
              background: "#242228",
              border: "1px solid #57535f",
              borderRadius: 4,
              color: "#e6e3f0",
            }}
            formatter={(value) => {
              const scalar = Array.isArray(value) ? value[0] : value;
              const amount =
                typeof scalar === "number" ? scalar : Number(scalar ?? 0);
              return metric === "cost"
                ? formatCost(amount)
                : formatCount(amount);
            }}
          />
          <Area
            type="monotone"
            dataKey={key}
            stroke={chartTheme.lilac}
            strokeWidth={1.75}
            fill={`url(#usage-${metric})`}
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

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
