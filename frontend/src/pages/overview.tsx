import { ArrowSquareOutIcon } from "@phosphor-icons/react";
import { useMemo, useState } from "react";
import { useOutletContext, useSearchParams } from "react-router-dom";

import { useAPI } from "../api";
import {
  ActivityHeatmap,
  type HeatmapMetric,
} from "../components/activity-heatmap";
import { AsyncView } from "../components/async-view";
import { RangeTabs } from "../components/range-tabs";
import { Stat, StatStrip } from "../components/stat-strip";
import { HoverTip, TokenCard } from "../components/token-card";
import {
  formatCost,
  formatCount,
  formatPercent,
  formatTokens,
} from "../format";
import "../overview.css";
import type { Analytics, Breakdown, DateRange, User, Viewer } from "../types";

type OverviewResponse = {
  range: string;
  ranges: DateRange[];
  users: User[];
  selected_user_ids: number[] | null;
  analytics: Analytics;
};

// formatSavings mirrors the server's FmtSavings: a non-negative saving reads
// "saved $X", and the rare negative (cache written but never re-read enough
// to repay the write premium) reads "cost $X" on its magnitude, so the Cache
// tile stays honest without a stray minus sign. Incomplete appends "partial"
// rather than the cost figures' "$X+" lower-bound marker, because an omitted
// model's saving can run either direction, not just under the shown value.
function formatSavings(usd: number, incomplete: boolean): string {
  const verb = usd < 0 ? "cost " : "saved ";
  const amount = formatCost(Math.abs(usd));
  return incomplete ? `${verb}${amount} partial` : `${verb}${amount}`;
}

export function AnalyticsPanel({
  analytics,
  showUsers = false,
}: {
  analytics: Analytics;
  // showUsers gates the by-user cost breakdown: the signed-in overview and
  // project pages opt in explicitly, while the public project surface keeps
  // the default off so the accounts that ran in a repo stay private.
  showUsers?: boolean;
}) {
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
  const hasData =
    analytics.Sessions > 0 ||
    analytics.TotalCost > 0 ||
    (analytics.Series?.length ?? 0) > 0;

  if (!hasData) {
    return (
      <section className="usage-empty">
        <p>No usage recorded yet.</p>
      </section>
    );
  }

  return (
    <>
      <StatStrip>
        <Stat
          label="Cost"
          value={formatCost(analytics.TotalCost, analytics.CostIncomplete)}
        />
        <Stat
          label="Tokens"
          value={
            <HoverTip summary={formatCount(totalTokens)}>
              <TokenCard
                input={analytics.TotalIn}
                output={analytics.TotalOut}
                cacheRead={analytics.TotalCacheRead}
                cacheWrite={analytics.TotalCacheWrite}
                reasoning={analytics.TotalReasoning}
              />
            </HoverTip>
          }
        />
        <Stat label="Sessions" value={formatCount(analytics.Sessions)} />
        <Stat
          label="Cache hit"
          value={
            <HoverTip summary={formatPercent(hitRate)}>
              <dl className="tt-grid">
                <dt>Hit rate</dt>
                <dd>{formatPercent(hitRate)}</dd>
                <dt>Cache read</dt>
                <dd>{formatTokens(analytics.Cache.CacheRead)}</dd>
                <dt>Cache write</dt>
                <dd>{formatTokens(analytics.Cache.CacheWrite)}</dd>
                <dt>Uncached in</dt>
                <dd>{formatTokens(analytics.Cache.Input)}</dd>
              </dl>
              <div className="tt-cost">
                {formatSavings(
                  analytics.Cache.SavingsUSD,
                  analytics.Cache.SavingsIncomplete,
                )}
              </div>
            </HoverTip>
          }
        />
      </StatStrip>
      <ActivityPanel analytics={analytics} />
      <div className="usage-breakdowns">
        <BreakdownTable title="Models" rows={analytics.Models ?? []} />
        <BreakdownTable title="Agents" rows={analytics.Agents ?? []} />
        {showUsers && (analytics.Users?.length ?? 0) > 1 ? (
          <BreakdownTable title="Users" rows={analytics.Users ?? []} />
        ) : null}
      </div>
    </>
  );
}

// ActivityPanel is the calendar activity grid with its Tokens/Dollars toggle:
// one cell per day over a trailing year, intensity scaled by the chosen metric.
export function ActivityPanel({ analytics }: { analytics: Analytics }) {
  const [metric, setMetric] = useState<HeatmapMetric>("tokens");
  return (
    <section className="instrument">
      <div className="section-head">
        <div>
          <h2>Daily activity</h2>
          <p>One cell per day; darker cells carried more of the window.</p>
        </div>
        <fieldset className="segmented">
          <legend className="sr-only">Metric</legend>
          {(
            [
              ["tokens", "Tokens"],
              ["cost", "Dollars"],
            ] as const
          ).map(([key, label]) => (
            <button
              type="button"
              key={key}
              className={metric === key ? "active" : ""}
              aria-pressed={metric === key}
              onClick={() => setMetric(key)}
            >
              {label}
            </button>
          ))}
        </fieldset>
      </div>
      <ActivityHeatmap series={analytics.Series} metric={metric} />
    </section>
  );
}

function BreakdownTable({ title, rows }: { title: string; rows: Breakdown[] }) {
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
          {rows.map((row) => {
            const tokens =
              row.Input + row.Output + row.CacheRead + row.CacheWrite;
            return (
              <div className="breakdown-row-wrap" key={row.Label}>
                <div className="breakdown-row">
                  <span className="breakdown-label">
                    {row.Label || "unknown"}
                  </span>
                  <span className="bar-track">
                    <span style={{ width: `${(row.CostUSD / max) * 100}%` }} />
                  </span>
                  <span className="data">
                    {formatCost(row.CostUSD, row.CostIncomplete)}
                  </span>
                </div>
                <div className="breakdown-sub">
                  <HoverTip summary={formatTokens(tokens)} className="tok-cell">
                    <TokenCard
                      input={row.Input}
                      output={row.Output}
                      cacheRead={row.CacheRead}
                      cacheWrite={row.CacheWrite}
                      reasoning={row.Reasoning}
                      costUSD={row.CostUSD}
                      costIncomplete={row.CostIncomplete}
                    />
                  </HoverTip>
                  <span>
                    {" "}
                    tok · {row.Sessions} session{row.Sessions === 1 ? "" : "s"}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

export function OverviewPage() {
  const viewer = useOutletContext<Viewer>();
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
      {viewer.overview_public && viewer.username ? (
        // A second entry point to the publicity control, so a published overview
        // is discoverable from the overview itself and not only buried in the
        // account settings. The chip links to the public view (scoped to this
        // one user), and Manage jumps to the account control to toggle it off.
        <div className="overview-public-badge">
          <a
            className="tag public"
            href={`/u/${encodeURIComponent(viewer.username)}`}
            target="_blank"
            rel="noopener"
            title="Open the public page in a new tab"
          >
            public <ArrowSquareOutIcon size={10} />
          </a>
          <a className="muted-link" href="/account#publicity">
            Manage
          </a>
        </div>
      ) : null}
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
            <AnalyticsPanel analytics={data.analytics} showUsers />
          </>
        )}
      </AsyncView>
    </div>
  );
}
