import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { RequestError, requestWithRetry, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { PublicShell } from "../components/public-shell";
import { RangeTabs } from "../components/range-tabs";
import { KindTag } from "../components/session-tags";
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
import "./public.css";
import { withBase } from "../base";
import { ToolsInstrument } from "../components/insights/tools";
import { TooltipHost } from "../components/insights/tooltip";
import { InsightsPanel } from "./insights";
import { AnalyticsPanel } from "./overview";
import { Transcript } from "./sessions";

// PublicPage wraps every logged-out surface with the shared shell, the page
// container, and a swap of AsyncView's generic error card for one that fits
// a public URL: a bad or revoked id is a dead end, not a transient failure
// worth a retry button (old PublicErrorPage, public.templ).
function PublicPage({ children }: { children: ReactNode }) {
  return (
    <PublicShell>
      <div className="page public-page">{children}</div>
    </PublicShell>
  );
}

function PublicErrorState({ error }: { error: Error }) {
  const notFound = error instanceof RequestError && error.status === 404;
  return (
    <section className="empty-state" role="alert">
      <h2>{notFound ? "Not found" : "Could not load this page"}</h2>
      <p>{error.message}</p>
      <a className="button secondary" href={withBase("/")}>
        Go to akari
      </a>
    </section>
  );
}

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
    <PublicPage>
      <AsyncView
        state={state}
        renderError={(error) => <PublicErrorState error={error} />}
      >
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
    </PublicPage>
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
    <PublicPage>
      <AsyncView
        state={state}
        renderError={(error) => <PublicErrorState error={error} />}
      >
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
              <TooltipHost>
                <ToolsInstrument
                  insights={data.insights}
                  resetKey={`${id}:${data.range}`}
                />
              </TooltipHost>
            </div>
          </>
        )}
      </AsyncView>
    </PublicPage>
  );
}

type PublicSessionResponse = { snapshot: PublicSessionSnapshot };

export function PublicSessionPage() {
  const { publicId = "" } = useParams();
  const state = useAPI<PublicSessionResponse>(
    `/api/v1/app/public/sessions/${encodeURIComponent(publicId)}`,
  );
  return (
    <PublicPage>
      <AsyncView
        state={state}
        renderError={(error) => <PublicErrorState error={error} />}
      >
        {(data) => {
          const detail = data.snapshot.Audit.Detail;
          const subagents = data.snapshot.Audit.Subagents ?? [];
          return (
            <>
              <header className="page-head">
                <div>
                  <div className="tag-row">
                    <span className="tag public">published session</span>
                    <span className="tag agent">{detail.Agent}</span>
                    <KindTag kind={detail.ProjectKind} />
                  </div>
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
                  value={formatCost(detail.TotalCostUSD, detail.CostIncomplete)}
                />
                <Stat label="Agent" value={detail.Agent} />
              </StatStrip>
              {subagents.length > 0 ? (
                <>
                  <h2>Subagents</h2>
                  <div className="feed">
                    {subagents
                      .filter((sub) => sub.PublicID)
                      .map((sub) => (
                        <Link
                          key={sub.ID}
                          to={`/s/${sub.PublicID}`}
                          className="feed-row"
                        >
                          <div className="feed-main">
                            <strong>{sub.Agent} subagent</strong>
                          </div>
                          <div className="feed-signals">
                            <span className="tag agent">{sub.Agent}</span>
                          </div>
                          <div className="feed-numbers">
                            <span>
                              {formatCount(sub.MessageCount)} messages
                            </span>
                          </div>
                        </Link>
                      ))}
                  </div>
                </>
              ) : null}
              <Transcript
                initial={data.snapshot.Page}
                blobBase={withBase(`/s/${publicId}/blob`)}
                loadEarlier={async (before) =>
                  (
                    await requestWithRetry<PublicSessionResponse>(
                      `/api/v1/app/public/sessions/${encodeURIComponent(publicId)}/transcript?before=${before}`,
                    )
                  ).snapshot.Page
                }
              />
            </>
          );
        }}
      </AsyncView>
    </PublicPage>
  );
}
