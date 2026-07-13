import { useParams, useSearchParams } from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { PublicShell } from "../components/public-shell";
import { RangeTabs } from "../components/range-tabs";
import { Stat, StatStrip } from "../components/stat-strip";
import {
  formatCost,
  formatCount,
  relativeTime,
  sessionTokens,
} from "../format";
import type {
  Analytics,
  DateRange,
  Insights,
  Project,
  PublicSessionSnapshot,
} from "../types";
import { InsightsPanel } from "./insights";
import { AnalyticsPanel } from "./overview";
import { Transcript } from "./sessions";

type PublicOverviewResponse = {
  username: string;
  range: string;
  ranges: DateRange[];
  analytics: Analytics;
};

export function PublicOverviewPage() {
  const { username = "" } = useParams();
  const [params] = useSearchParams();
  const state = useAPI<PublicOverviewResponse>(
    `/api/v1/app/public/users/${encodeURIComponent(username)}?${params.toString()}`,
  );
  return (
    <PublicShell>
      <div className="page public-page">
        <AsyncView state={state}>
          {(data) => (
            <>
              <header className="page-head">
                <div>
                  <span className="tag public">published</span>
                  <h1>{data.username} / usage</h1>
                  <p>AI coding-agent activity shared from akari.</p>
                </div>
                <RangeTabs ranges={data.ranges} active={data.range} />
              </header>
              <AnalyticsPanel analytics={data.analytics} />
            </>
          )}
        </AsyncView>
      </div>
    </PublicShell>
  );
}

type PublicProjectResponse = {
  project: Project;
  range: string;
  ranges: DateRange[];
  analytics: Analytics;
  insights: Insights;
};

export function PublicProjectPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const state = useAPI<PublicProjectResponse>(
    `/api/v1/app/public/projects/${encodeURIComponent(id)}?${params.toString()}`,
  );
  return (
    <PublicShell>
      <div className="page public-page">
        <AsyncView state={state}>
          {(data) => (
            <>
              <header className="page-head">
                <div>
                  <span className="tag public">published</span>
                  <h1>
                    {data.project.DisplayName || data.project.RemoteKey} / usage
                  </h1>
                  <p>{data.project.RemoteKey}</p>
                </div>
                <RangeTabs ranges={data.ranges} active={data.range} />
              </header>
              <AnalyticsPanel analytics={data.analytics} />
              <div className="project-insights">
                <h2>Quality signals</h2>
                <InsightsPanel insights={data.insights} />
              </div>
            </>
          )}
        </AsyncView>
      </div>
    </PublicShell>
  );
}

type PublicSessionResponse = { snapshot: PublicSessionSnapshot };

export function PublicSessionPage() {
  const { publicId = "" } = useParams();
  const state = useAPI<PublicSessionResponse>(
    `/api/v1/app/public/sessions/${encodeURIComponent(publicId)}`,
  );
  return (
    <PublicShell>
      <div className="page public-page">
        <AsyncView state={state}>
          {(data) => {
            const detail = data.snapshot.Audit.Detail;
            return (
              <>
                <header className="page-head">
                  <div>
                    <span className="tag public">published session</span>
                    <h1>{detail.Title || `${detail.Agent} session`}</h1>
                    <p>
                      {detail.ProjectName || detail.ProjectKey} ·{" "}
                      {detail.Username} · {relativeTime(detail.StartedAt)}
                    </p>
                  </div>
                </header>
                <StatStrip>
                  <Stat
                    label="Messages"
                    value={formatCount(detail.MessageCount)}
                  />
                  <Stat
                    label="Tokens"
                    value={formatCount(sessionTokens(detail))}
                  />
                  <Stat
                    label="Cost"
                    value={formatCost(
                      detail.TotalCostUSD,
                      detail.CostIncomplete,
                    )}
                  />
                  <Stat label="Agent" value={detail.Agent} />
                </StatStrip>
                <Transcript
                  initial={data.snapshot.Page}
                  blobBase={`/s/${publicId}/blob`}
                  loadEarlier={async (before) =>
                    (
                      await request<PublicSessionResponse>(
                        `/api/v1/app/public/sessions/${encodeURIComponent(publicId)}/transcript?before=${before}`,
                      )
                    ).snapshot.Page
                  }
                />
              </>
            );
          }}
        </AsyncView>
      </div>
    </PublicShell>
  );
}
