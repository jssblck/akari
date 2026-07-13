import { useSearchParams } from "react-router-dom";

import { useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { DistributionChart } from "../components/charts";
import { RangeTabs } from "../components/range-tabs";
import { Stat, StatStrip } from "../components/stat-strip";
import { formatCount, formatPercent } from "../format";
import type { DateRange, Insights } from "../types";

type InsightsResponse = {
  range: string;
  ranges: DateRange[];
  generated_at: string;
  insights: Insights;
};

export function InsightsPanel({ insights }: { insights: Insights }) {
  const coverage =
    insights.Quality.Sessions === 0
      ? 0
      : insights.Quality.Graded / insights.Quality.Sessions;
  return (
    <>
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
            insights.Archetypes.filter((item) => item.Count > 0).length,
          )}
        />
        <Stat
          label="Context resets"
          value={formatCount(insights.Context.TotalResets ?? 0)}
        />
      </StatStrip>
      <div className="split-panels">
        <section className="instrument">
          <div className="section-head">
            <div>
              <h2>Grades</h2>
              <p>Current settled-session quality scores.</p>
            </div>
          </div>
          <DistributionChart
            data={insights.Quality.Grades}
            label="Grade distribution"
          />
        </section>
        <section className="instrument">
          <div className="section-head">
            <div>
              <h2>Outcomes</h2>
              <p>How sessions ended in this window.</p>
            </div>
          </div>
          <DistributionChart
            data={insights.Quality.Outcomes}
            label="Outcome distribution"
          />
        </section>
      </div>
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
    </>
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
        {(data) => (
          <>
            <header className="page-head">
              <div>
                <h1>Insights</h1>
                <p>
                  Quality, cadence, tool health, context pressure, and
                  delegation.
                </p>
              </div>
              <RangeTabs ranges={data.ranges} active={data.range} />
            </header>
            <InsightsPanel insights={data.insights} />
          </>
        )}
      </AsyncView>
    </div>
  );
}
