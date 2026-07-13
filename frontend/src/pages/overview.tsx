import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";

import { useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { UsageChart } from "../components/charts";
import { RangeTabs } from "../components/range-tabs";
import { Stat, StatStrip } from "../components/stat-strip";
import { formatCost, formatCount, formatPercent } from "../format";
import type { Analytics, DateRange, User } from "../types";

type OverviewResponse = {
  range: string;
  ranges: DateRange[];
  users: User[];
  selected_user_ids: number[] | null;
  analytics: Analytics;
};

export function AnalyticsPanel({ analytics }: { analytics: Analytics }) {
  const promptTokens =
    analytics.Cache.Input +
    analytics.Cache.CacheRead +
    analytics.Cache.CacheWrite;
  const hitRate =
    promptTokens === 0 ? 0 : analytics.Cache.CacheRead / promptTokens;
  const totalTokens =
    analytics.TotalIn +
    analytics.TotalOut +
    analytics.TotalCacheRead +
    analytics.TotalCacheWrite;
  return (
    <>
      <StatStrip>
        <Stat
          label="Cost"
          value={formatCost(analytics.TotalCost, analytics.CostIncomplete)}
        />
        <Stat
          label="Tokens"
          value={formatCount(totalTokens)}
          note={`${formatCount(analytics.TotalReasoning)} reasoning`}
        />
        <Stat label="Sessions" value={formatCount(analytics.Sessions)} />
        <Stat
          label="Cache hit"
          value={formatPercent(hitRate)}
          note={`${formatCost(analytics.Cache.SavingsUSD)} saved`}
        />
      </StatStrip>
      <section className="instrument">
        <div className="section-head">
          <div>
            <h2>Usage</h2>
            <p>Daily cost across every dated usage event in this scope.</p>
          </div>
        </div>
        <UsageChart analytics={analytics} />
      </section>
      <div className="split-panels">
        <BreakdownTable title="Models" rows={analytics.Models ?? []} />
        <BreakdownTable title="Agents" rows={analytics.Agents ?? []} />
      </div>
    </>
  );
}

function BreakdownTable({
  title,
  rows,
}: {
  title: string;
  rows: Analytics["Models"] extends infer _
    ? NonNullable<Analytics["Models"]>
    : never;
}) {
  const max = Math.max(...rows.map((row) => row.CostUSD), 0.0001);
  return (
    <section className="instrument compact">
      <div className="section-head">
        <h2>{title}</h2>
      </div>
      {rows.length === 0 ? (
        <p className="empty-inline">No usage in this window.</p>
      ) : (
        <div className="breakdown-list">
          {rows.map((row) => (
            <div className="breakdown-row" key={row.Label}>
              <span className="breakdown-label">{row.Label || "unknown"}</span>
              <span className="bar-track">
                <span style={{ width: `${(row.CostUSD / max) * 100}%` }} />
              </span>
              <span className="data">
                {formatCost(row.CostUSD, row.CostIncomplete)}
              </span>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

export function OverviewPage() {
  const [params, setParams] = useSearchParams();
  const path = `/api/v1/app/overview?${params.toString()}`;
  const state = useAPI<OverviewResponse>(path);
  const selected = useMemo(
    () =>
      new Set(
        state.kind === "ready" ? (state.data.selected_user_ids ?? []) : [],
      ),
    [state],
  );
  return (
    <div className="page">
      <AsyncView state={state}>
        {(data) => (
          <>
            <header className="page-head">
              <div>
                <h1>Overview</h1>
                <p>
                  Fleet usage across projects, models, agents, and accounts.
                </p>
              </div>
              <RangeTabs ranges={data.ranges} active={data.range} />
            </header>
            {data.users.length > 1 ? (
              <fieldset className="filter-row">
                <legend className="sr-only">Account filter</legend>
                <span className="label">Accounts</span>
                {data.users.map((user) => (
                  <button
                    type="button"
                    key={user.ID}
                    className={
                      selected.has(user.ID)
                        ? "filter-chip active"
                        : "filter-chip"
                    }
                    onClick={() => {
                      const next = new URLSearchParams(params);
                      const values = new Set(next.getAll("user"));
                      const value = String(user.ID);
                      if (values.has(value)) values.delete(value);
                      else values.add(value);
                      next.delete("user");
                      for (const id of values) next.append("user", id);
                      setParams(next);
                    }}
                  >
                    {user.Username}
                  </button>
                ))}
              </fieldset>
            ) : null}
            <AnalyticsPanel analytics={data.analytics} />
          </>
        )}
      </AsyncView>
    </div>
  );
}
