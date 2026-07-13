import { ArrowSquareOutIcon, GitBranchIcon } from "@phosphor-icons/react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { Sparkline } from "../components/charts";
import { RangeTabs } from "../components/range-tabs";
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
  SessionSummary,
} from "../types";
import { InsightsPanel } from "./insights";
import { AnalyticsPanel } from "./overview";

type ProjectsResponse = {
  projects: Project[];
  sparklines: Record<string, number[]>;
};

function projectLabel(project: Project): string {
  return project.DisplayName || project.RemoteKey || `Project ${project.ID}`;
}

export function ProjectsPage() {
  const state = useAPI<ProjectsResponse>("/api/v1/app/projects");
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Projects</h1>
          <p>Repositories and local workspaces, ordered by recent activity.</p>
        </div>
      </header>
      <AsyncView state={state}>
        {(data) =>
          data.projects.length === 0 ? (
            <section className="empty-state">
              <h2>No projects yet</h2>
              <p>
                Run an akari client sync to create the first project and
                session.
              </p>
              <a className="button" href="/guide">
                Read the setup guide
              </a>
            </section>
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Project</th>
                    <th>Kind</th>
                    <th className="num">Sessions</th>
                    <th className="num">Tokens</th>
                    <th>30-day activity</th>
                    <th className="num">Cost</th>
                    <th>Last active</th>
                  </tr>
                </thead>
                <tbody>
                  {data.projects.map((project) => (
                    <tr key={project.ID}>
                      <td>
                        <Link
                          className="primary-link"
                          to={`/projects/${project.ID}`}
                        >
                          {projectLabel(project)}
                        </Link>
                        <span className="cell-note">{project.RemoteKey}</span>
                      </td>
                      <td>
                        <span className="tag">
                          <GitBranchIcon size={13} /> {project.Kind}
                        </span>
                      </td>
                      <td className="num">
                        {formatCount(project.SessionCount)}
                      </td>
                      <td className="num">
                        {formatCount(
                          project.TotalInput +
                            project.TotalOutput +
                            project.TotalCacheRead +
                            project.TotalCacheWrite,
                        )}
                      </td>
                      <td>
                        <Sparkline
                          values={data.sparklines[String(project.ID)]}
                        />
                      </td>
                      <td className="num">
                        {formatCost(
                          project.TotalCostUSD,
                          project.CostIncomplete,
                        )}
                      </td>
                      <td>{relativeTime(project.LastActivity)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </AsyncView>
    </div>
  );
}

type ProjectResponse = {
  project: Project;
  range: string;
  ranges: DateRange[];
  sessions: SessionSummary[] | null;
  remainder: { Sessions: number; CostUSD: number; CostIncomplete: boolean };
  facets: {
    Agents: string[] | null;
    Machines: string[] | null;
    Users: string[] | null;
  };
  analytics: Analytics;
  insights: Insights;
};

export function ProjectPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const path = `/api/v1/app/projects/${encodeURIComponent(id)}?${params.toString()}`;
  const state = useAPI<ProjectResponse>(path);
  return (
    <div className="page">
      <AsyncView state={state}>
        {(data) => (
          <>
            <header className="page-head project-head">
              <div>
                <span className="crumb">
                  <Link to="/projects">Projects</Link> /{" "}
                  {data.project.Host || "local"}
                </span>
                <h1>{projectLabel(data.project)}</h1>
                <p>{data.project.RemoteKey}</p>
              </div>
              <div className="head-actions">
                <RangeTabs ranges={data.ranges} active={data.range} />
                <button
                  type="button"
                  className="button secondary"
                  onClick={async () => {
                    await request(
                      `/api/v1/app/projects/${data.project.ID}/publication`,
                      {
                        method: "PUT",
                        body: JSON.stringify({
                          published: !data.project.OverviewPublic,
                        }),
                      },
                    );
                    window.location.reload();
                  }}
                >
                  {data.project.OverviewPublic
                    ? "Make private"
                    : "Publish overview"}
                </button>
                {data.project.OverviewPublic ? (
                  <a
                    className="icon-link"
                    href={`/p/${data.project.ID}`}
                    target="_blank"
                    rel="noreferrer"
                    aria-label="Open public overview"
                  >
                    <ArrowSquareOutIcon />
                  </a>
                ) : null}
              </div>
            </header>
            <AnalyticsPanel analytics={data.analytics} />
            <section className="instrument compact">
              <div className="section-head">
                <div>
                  <h2>Sessions</h2>
                  <p>Usage-bearing sessions inside the selected window.</p>
                </div>
              </div>
              {(data.sessions ?? []).length === 0 ? (
                <p className="empty-inline">No sessions in this scope.</p>
              ) : (
                <div className="table-wrap embedded">
                  <table className="data-table">
                    <thead>
                      <tr>
                        <th>Session</th>
                        <th>Agent</th>
                        <th className="num">Messages</th>
                        <th className="num">Tokens</th>
                        <th className="num">Cost</th>
                        <th>Active</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(data.sessions ?? []).map((session) => (
                        <tr key={session.ID}>
                          <td>
                            <Link
                              className="primary-link"
                              to={`/sessions/${session.ID}`}
                            >
                              {session.Title || `Session ${session.ID}`}
                            </Link>
                          </td>
                          <td>
                            <span className="tag agent">{session.Agent}</span>
                          </td>
                          <td className="num">
                            {formatCount(session.MessageCount)}
                          </td>
                          <td className="num">
                            {formatCount(sessionTokens(session))}
                          </td>
                          <td className="num">
                            {formatCost(
                              session.TotalCostUSD,
                              session.CostIncomplete,
                            )}
                          </td>
                          <td>{relativeTime(session.LastActiveAt)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
            <div className="project-insights">
              <h2>Quality signals</h2>
              <InsightsPanel insights={data.insights} />
            </div>
          </>
        )}
      </AsyncView>
    </div>
  );
}
