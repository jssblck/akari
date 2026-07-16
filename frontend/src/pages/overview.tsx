import { ArrowSquareOutIcon } from "@phosphor-icons/react";
import { type ReactNode, useState } from "react";
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
import { withBase } from "../base";
import type {
  Analytics,
  Breakdown,
  OverviewResponse,
  User,
  Viewer,
} from "../types";

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
  activityControls,
  mobileActivity = "full",
}: {
  analytics: Analytics;
  // showUsers gates the by-user cost breakdown: the signed-in overview and
  // project pages opt in explicitly, while the public project surface keeps
  // the default off so the accounts that ran in a repo stay private.
  showUsers?: boolean;
  // activityControls render in the Daily activity header row, directly under
  // the stat strip. The overview page slots its range picker and account
  // filter here; pages that keep those controls in their own header pass
  // nothing.
  activityControls?: ReactNode;
  mobileActivity?: "full" | "range-only";
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
    // The controls stay reachable above the empty card: a window with no
    // usage must still offer the range picker as the way back out.
    return (
      <>
        {activityControls ? (
          <div className="activity-controls standalone">{activityControls}</div>
        ) : null}
        <section className="usage-empty">
          <p>No usage recorded yet.</p>
        </section>
      </>
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
        <Stat label="Sessions" value={formatCount(analytics.Sessions)} />
      </StatStrip>
      <ActivityPanel
        analytics={analytics}
        controls={activityControls}
        mobileActivity={mobileActivity}
      />
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
export function ActivityPanel({
  analytics,
  controls,
  mobileActivity = "full",
}: {
  analytics: Analytics;
  controls?: ReactNode;
  mobileActivity?: "full" | "range-only";
}) {
  const [metric, setMetric] = useState<HeatmapMetric>("tokens");
  return (
    <section className={`instrument activity-panel ${mobileActivity}`}>
      <div className="section-head">
        <h2>Daily activity</h2>
        <div className="activity-controls">
          {controls}
          <fieldset className="segmented activity-metric">
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
          {rows.map((row, index) => {
            const tokens =
              row.Input + row.Output + row.CacheRead + row.CacheWrite;
            return (
              <div className="breakdown-row" key={row.Label}>
                <span
                  className="breakdown-fill"
                  style={{
                    width: `${Math.max((row.CostUSD / max) * 100, 1)}%`,
                    background: `var(--viz-${(index % 8) + 1})`,
                  }}
                />
                <div className="breakdown-head">
                  <span className="breakdown-label">
                    {row.Label || "unknown"}
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

// AccountFilter narrows the whole overview to one account, mirroring the
// project page's auto-applying select filters. The URL param is the source of
// truth so the control reflects a click immediately, before the refetch lands.
function AccountFilter({ users }: { users: User[] }) {
  const [params, setParams] = useSearchParams();
  const selected = params.getAll("user");
  // The API still honors repeated user params (an old bookmark, a hand-built
  // link). The select cannot express that set, so it names the state honestly
  // with a disabled placeholder instead of claiming "All accounts" while the
  // data underneath is filtered; any pick replaces the whole set.
  const value =
    selected.length === 1 ? selected[0] : selected.length > 1 ? "%multi" : "";
  return (
    <select
      aria-label="Account filter"
      className={value ? "active" : ""}
      value={value}
      onChange={(event) => {
        const next = new URLSearchParams(params);
        next.delete("user");
        if (event.target.value) next.append("user", event.target.value);
        setParams(next);
      }}
    >
      <option value="">All accounts</option>
      {selected.length > 1 ? (
        <option value="%multi" disabled>
          {selected.length} accounts
        </option>
      ) : null}
      {users.map((user) => (
        <option key={user.ID} value={String(user.ID)}>
          {user.Username}
        </option>
      ))}
    </select>
  );
}

export function OverviewPage() {
  const viewer = useOutletContext<Viewer>();
  const [params] = useSearchParams();
  const path = `/api/v1/app/overview?${params.toString()}`;
  const state = useAPI<OverviewResponse>(path);
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
            href={withBase(`/u/${encodeURIComponent(viewer.username)}`)}
            target="_blank"
            rel="noopener"
            title="Open the public page in a new tab"
          >
            public <ArrowSquareOutIcon size={10} />
          </a>
          <a className="muted-link" href={withBase("/account#publicity")}>
            Manage
          </a>
        </div>
      ) : null}
      <AsyncView state={state}>
        {(data) => (
          <AnalyticsPanel
            analytics={data.analytics}
            showUsers
            mobileActivity="range-only"
            activityControls={
              <>
                {(data.users ?? []).length > 1 ? (
                  <AccountFilter users={data.users ?? []} />
                ) : null}
                <RangeTabs ranges={data.ranges ?? []} active={data.range} />
              </>
            }
          />
        )}
      </AsyncView>
    </div>
  );
}
