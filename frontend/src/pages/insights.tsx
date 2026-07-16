import { useSearchParams } from "react-router-dom";

import { useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { EconomicsInstrument } from "../components/insights/economics";
import { FleetMixInstrument } from "../components/insights/fleet-mix";
import {
  GRADE_ORDER,
  gradeColor,
  gradeLabel,
} from "../components/insights/format";
import { SessionGalleryInstrument } from "../components/insights/gallery";
import {
  ArchetypesChart,
  GradesChart,
  HealthInstrument,
  OutcomesChart,
} from "../components/insights/health";
import { Legend } from "../components/insights/legend";
import { SubagentsInstrument } from "../components/insights/subagents";
import { ToolsInstrument } from "../components/insights/tools";
import { TooltipHost } from "../components/insights/tooltip";
import { VelocityInstrument } from "../components/insights/velocity";
import "../insights.css";
import { RangeTabs } from "../components/range-tabs";
import { Stat, StatStrip } from "../components/stat-strip";
import { formatCount, formatPercent } from "../format";
import { normalizeInsights } from "../normalize-insights";
import type { Insights, InsightsResponse } from "../types";

// InsightsPanel is the project (and public-project) quality band: three
// signal-trend charts (Grades, Outcomes, session shape) plus the reusable
// Tools instrument, over store.QualityBandPanels' narrower payload (Tools +
// Churn, with the signal trends riding along for free on every bucketed
// call). Its props stay { insights: Insights } so projects.tsx and
// public.tsx keep working unmodified.
export function InsightsPanel({ insights }: { insights: Insights }) {
  const coverage =
    insights.Quality.Sessions === 0
      ? 0
      : insights.Quality.Graded / insights.Quality.Sessions;
  const trends = insights.Trends;
  const hasTrend = !!trends && trends.BucketStarts.length > 0;
  const n = trends?.BucketStarts.length ?? 0;

  return (
    <TooltipHost>
      <StatStrip>
        <Stat label="Sessions" value={formatCount(insights.Quality.Sessions)} />
        <Stat
          label="Graded"
          value={formatPercent(coverage)}
          note={`${formatCount(insights.Quality.Graded)} measured`}
        />
        <Stat
          label="Archetypes"
          value={formatCount(
            (insights.Archetypes ?? []).filter((item) => item.Count > 0).length,
          )}
        />
        <Stat
          label="Context resets"
          value={formatCount(insights.Context.TotalResets ?? 0)}
        />
      </StatStrip>
      {hasTrend && trends ? (
        <>
          <div className="split-panels">
            <section className="instrument">
              <div className="section-head">
                <div>
                  <h2>Grades</h2>
                  <p>How session quality graded over the window.</p>
                </div>
              </div>
              <GradesChart
                signals={trends.Signals}
                n={n}
                labels={trends.Labels}
                mini={false}
              />
              <Legend
                items={GRADE_ORDER.map((key) => ({
                  color: gradeColor(key),
                  label: gradeLabel(key),
                }))}
              />
            </section>
            <section className="instrument">
              <div className="section-head">
                <div>
                  <h2>Outcomes</h2>
                  <p>How sessions ended over the window.</p>
                </div>
              </div>
              <OutcomesChart
                signals={trends.Signals}
                n={n}
                labels={trends.Labels}
                mini={false}
              />
            </section>
          </div>
          <section className="instrument">
            <div className="section-head">
              <div>
                <h2>Session shape</h2>
                <p>
                  Runs grouped by measured length and turn count, over the
                  window.
                </p>
              </div>
            </div>
            <ArchetypesChart
              signals={trends.Signals}
              n={n}
              labels={trends.Labels}
            />
          </section>
          <ToolsInstrument insights={insights} resetKey={insights} />
        </>
      ) : (
        <section className="instrument compact">
          <div className="section-head">
            <div>
              <h2>Session shape</h2>
              <p>Runs grouped by measured length and turn count.</p>
            </div>
          </div>
          <div className="distribution-bars">
            {insights.Archetypes.map((item) => {
              const width =
                insights.Quality.Sessions > 0
                  ? (item.Count / insights.Quality.Sessions) * 100
                  : 0;
              return (
                <div className="distribution-row" key={item.Key}>
                  <span>{item.Key}</span>
                  <span className="bar-track">
                    <span style={{ width: `${width}%` }} />
                  </span>
                  <strong>{formatCount(item.Count)}</strong>
                </div>
              );
            })}
          </div>
        </section>
      )}
    </TooltipHost>
  );
}

function EmptyInsights() {
  return (
    <div className="insights-empty" role="status">
      No sessions in this window yet.
    </div>
  );
}

export function InsightsPage() {
  const [params] = useSearchParams();
  const state = useAPI<InsightsResponse>(
    `/api/v1/app/insights?${params.toString()}`,
  );
  return (
    <div className="page">
      <AsyncView state={state}>
        {(data) => {
          const insights = normalizeInsights(data.insights);
          const trends = insights.Trends;
          const hasData = !!trends && trends.BucketStarts.length > 0;
          return (
            <>
              <header className="page-head">
                <RangeTabs ranges={data.ranges ?? []} active={data.range} />
              </header>
              {!hasData || !trends ? (
                <EmptyInsights />
              ) : (
                <TooltipHost>
                  <FleetMixInstrument trends={trends} />
                  <SessionGalleryInstrument trends={trends} />
                  <VelocityInstrument
                    insights={insights}
                    trends={trends}
                    resetKey={data.range}
                  />
                  <ToolsInstrument insights={insights} resetKey={data.range} />
                  <HealthInstrument
                    insights={insights}
                    trends={trends}
                    resetKey={data.range}
                  />
                  <EconomicsInstrument trends={trends} resetKey={data.range} />
                  <SubagentsInstrument trends={trends} />
                </TooltipHost>
              )}
            </>
          );
        }}
      </AsyncView>
    </div>
  );
}
