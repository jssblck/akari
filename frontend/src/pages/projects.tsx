import { ArrowSquareOutIcon } from "@phosphor-icons/react";
import type { MouseEvent } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { Sparkline } from "../components/charts";
import { ToolsInstrument } from "../components/insights/tools";
import { TooltipHost } from "../components/insights/tooltip";
import { attempt } from "../components/notices";
import { RangeTabs } from "../components/range-tabs";
import { HoverTip, TokenCard } from "../components/token-card";
import {
  formatCost,
  formatCount,
  formatTime,
  relativeTime,
  sessionTokens,
} from "../format";
import "../projects.css";
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

// isLocalKind mirrors the server's IsLocalKind: a standalone or orphaned
// project has no git remote, so it groups and labels apart from a repository
// everywhere the UI distinguishes the two (the projects ledger, the
// publicity control, the session filter facets).
function isLocalKind(kind: string): boolean {
  return kind === "standalone" || kind === "orphaned";
}

function projectLabel(project: Project): string {
  return project.DisplayName || project.RemoteKey || `Project ${project.ID}`;
}

// localPath recovers the working-directory path from a local project's
// synthetic remote key ("local:machine:path"), the same string the server's
// LocalPath helper derives, for display beside the folder name.
function localPath(project: Project): string {
  if (!isLocalKind(project.Kind)) return "";
  const prefix = `local:${project.Host}:`;
  return project.RemoteKey.startsWith(prefix)
    ? project.RemoteKey.slice(prefix.length)
    : project.RemoteKey;
}

// KindTag is the one state chip for a non-remote project; a remote project
// gets no chip since it is the default and needs no label.
function KindTag({ kind }: { kind: string }) {
  if (kind === "standalone") {
    return (
      <span className="tag" title="No .git, or no git origin remote.">
        standalone
      </span>
    );
  }
  if (kind === "orphaned") {
    return (
      <span
        className="tag warn"
        title="The working directory no longer exists on disk."
      >
        orphaned
      </span>
    );
  }
  return null;
}

function ProjectTokensCell({ project }: { project: Project }) {
  const total =
    project.TotalInput +
    project.TotalOutput +
    project.TotalCacheRead +
    project.TotalCacheWrite;
  return (
    <HoverTip summary={formatCount(total)} className="tok-cell">
      <TokenCard
        input={project.TotalInput}
        output={project.TotalOutput}
        cacheRead={project.TotalCacheRead}
        cacheWrite={project.TotalCacheWrite}
        costUSD={project.TotalCostUSD}
        costIncomplete={project.CostIncomplete}
      />
    </HoverTip>
  );
}

// ProjectRow makes the whole row a click target while keeping a real anchor
// for modifier-clicks (open in new tab, copy link): a plain click anywhere in
// the row navigates via the router, but a click on the anchor itself, or one
// carrying a modifier or a non-primary button, falls through to the
// anchor's native behavior instead of being hijacked.
function ProjectRow({
  project,
  sparkline,
  showKind,
}: {
  project: Project;
  sparkline: number[] | undefined;
  showKind: boolean;
}) {
  const navigate = useNavigate();
  const local = isLocalKind(project.Kind);
  const handleRowClick = (event: MouseEvent<HTMLTableRowElement>) => {
    if (event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey)
      return;
    if ((event.target as HTMLElement).closest("a")) return;
    navigate(`/projects/${project.ID}`);
  };
  return (
    <tr className="row-link" onClick={handleRowClick}>
      <td>
        <Link className="primary-link" to={`/projects/${project.ID}`}>
          {local
            ? project.DisplayName || `Project ${project.ID}`
            : projectLabel(project)}
        </Link>
        <span className="cell-note">
          {local ? localPath(project) : project.RemoteKey}
        </span>
      </td>
      {showKind ? (
        <td>
          <KindTag kind={project.Kind} />
          {project.Host ? <span className="muted"> {project.Host}</span> : null}
        </td>
      ) : null}
      <td className="num">{formatCount(project.SessionCount)}</td>
      <td className="num">
        <ProjectTokensCell project={project} />
      </td>
      <td>
        <Sparkline values={sparkline} />
      </td>
      <td className="num">
        {formatCost(project.TotalCostUSD, project.CostIncomplete)}
      </td>
      <td>{relativeTime(project.LastActivity)}</td>
    </tr>
  );
}

// ProjectSection is one ledger of the projects index: Repositories or Local
// folders, each headed by its own count so a fleet with, say, no local
// scratch folders shows the repositories ledger alone rather than an empty
// second table.
function ProjectSection({
  title,
  projects,
  sparklines,
  showKind,
}: {
  title: string;
  projects: Project[];
  sparklines: Record<string, number[]>;
  showKind: boolean;
}) {
  if (projects.length === 0) return null;
  return (
    <section className="instrument compact proj-section">
      <div className="section-head">
        <h2>{title}</h2>
        <span className="muted">
          {formatCount(projects.length)} project
          {projects.length === 1 ? "" : "s"}
        </span>
      </div>
      <div className="table-wrap embedded">
        <table className="data-table">
          <thead>
            <tr>
              <th>Project</th>
              {showKind ? <th>Kind</th> : null}
              <th className="num">Sessions</th>
              <th className="num">Tokens</th>
              <th>30-day activity</th>
              <th className="num">Cost</th>
              <th>Last active</th>
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => (
              <ProjectRow
                key={project.ID}
                project={project}
                sparkline={sparklines[String(project.ID)]}
                showKind={showKind}
              />
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
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
            <>
              <ProjectSection
                title="Repositories"
                projects={data.projects.filter(
                  (project) => !isLocalKind(project.Kind),
                )}
                sparklines={data.sparklines}
                showKind={false}
              />
              <ProjectSection
                title="Local folders"
                projects={data.projects.filter((project) =>
                  isLocalKind(project.Kind),
                )}
                sparklines={data.sparklines}
                showKind
              />
            </>
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
  remainder: {
    Sessions: number;
    Input: number;
    Output: number;
    CacheRead: number;
    CacheWrite: number;
    CostUSD: number;
    CostIncomplete: boolean;
  };
  facets: {
    Agents: string[] | null;
    Machines: string[] | null;
    Users: string[] | null;
  };
  analytics: Analytics;
  insights: Insights;
};

// ProjectToolbar is the project page's session filter: three auto-applying
// selects (Agent, User, Machine) that narrow the whole scoped view (the usage
// panel and the session table both re-fetch under the chosen facets), reading
// and writing the same URL params the project API already accepts.
function ProjectToolbar({ facets }: { facets: ProjectResponse["facets"] }) {
  const [params, setParams] = useSearchParams();
  const update = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next);
  };
  return (
    <fieldset className="filter-row">
      <legend className="sr-only">Session filters</legend>
      <span className="label">Filter</span>
      <select
        aria-label="Agent"
        value={params.get("agent") ?? ""}
        onChange={(event) => update("agent", event.target.value)}
      >
        <option value="">All agents</option>
        {(facets.Agents ?? []).map((agent) => (
          <option key={agent} value={agent}>
            {agent}
          </option>
        ))}
      </select>
      <select
        aria-label="User"
        value={params.get("user") ?? ""}
        onChange={(event) => update("user", event.target.value)}
      >
        <option value="">All users</option>
        {(facets.Users ?? []).map((user) => (
          <option key={user} value={user}>
            {user}
          </option>
        ))}
      </select>
      <select
        aria-label="Machine"
        value={params.get("machine") ?? ""}
        onChange={(event) => update("machine", event.target.value)}
      >
        <option value="">All machines</option>
        {(facets.Machines ?? []).map((machine) => (
          <option key={machine} value={machine}>
            {machine}
          </option>
        ))}
      </select>
    </fieldset>
  );
}

// SessionPublicTag renders a session's public chip from its row fields:
// nothing while private, the linked chip when its public id is known, and
// the plain marker otherwise, matching the shared publicTag convention.
function SessionPublicTag({ session }: { session: SessionSummary }) {
  if (session.Visibility !== "public") return null;
  if (session.PublicID) {
    return (
      <a
        className="tag public"
        href={`/s/${session.PublicID}`}
        target="_blank"
        rel="noopener"
        title="Open the public page in a new tab"
      >
        public <ArrowSquareOutIcon size={10} />
      </a>
    );
  }
  return <span className="tag public">public</span>;
}

export function ProjectPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const path = `/api/v1/app/projects/${encodeURIComponent(id)}?${params.toString()}`;
  const state = useAPI<ProjectResponse>(path);
  return (
    <div className="page">
      <AsyncView state={state}>
        {(data) => {
          const local = isLocalKind(data.project.Kind);
          const remainderTokens =
            data.remainder.Input +
            data.remainder.Output +
            data.remainder.CacheRead +
            data.remainder.CacheWrite;
          const togglePublicity = async () => {
            const next = !data.project.OverviewPublic;
            const ok = await attempt(
              request(`/api/v1/app/projects/${data.project.ID}/publication`, {
                method: "PUT",
                body: JSON.stringify({ published: next }),
              }),
              next
                ? "Project overview published."
                : "Project overview made private.",
            );
            if (ok) window.location.reload();
          };
          return (
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
                  {!local ? (
                    data.project.OverviewPublic ? (
                      <>
                        <a
                          className="tag public"
                          href={`/p/${data.project.ID}`}
                          target="_blank"
                          rel="noopener"
                          title="Open the public page in a new tab"
                        >
                          public <ArrowSquareOutIcon size={10} />
                        </a>
                        <button
                          type="button"
                          className="button secondary"
                          onClick={togglePublicity}
                          title="Hide the public page. The URL is the project id, so making it public again brings the same link back."
                        >
                          Make private
                        </button>
                      </>
                    ) : (
                      <button
                        type="button"
                        className="button secondary"
                        onClick={togglePublicity}
                        title="Anyone can read this project's usage overview while it is public. Sessions stay private."
                      >
                        Make overview public
                      </button>
                    )
                  ) : null}
                </div>
              </header>
              <ProjectToolbar facets={data.facets} />
              <AnalyticsPanel analytics={data.analytics} showUsers />
              <section className="instrument compact">
                <div className="section-head">
                  <div>
                    <h2>Sessions</h2>
                    <p>Usage-bearing sessions inside the selected window.</p>
                  </div>
                </div>
                {(data.sessions ?? []).length === 0 ? (
                  <p className="empty-inline">
                    No sessions match these filters.
                  </p>
                ) : (
                  <div className="table-wrap embedded">
                    <table className="data-table">
                      <thead>
                        <tr>
                          <th>Session</th>
                          <th>Agent</th>
                          <th>User</th>
                          <th>Machine</th>
                          <th>Branch</th>
                          <th className="num">Messages</th>
                          <th className="num">Tokens</th>
                          <th className="num">Cost</th>
                          <th>Updated</th>
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
                              </Link>{" "}
                              <SessionPublicTag session={session} />
                            </td>
                            <td>
                              <span className="tag agent">{session.Agent}</span>
                            </td>
                            <td className="muted">{session.Username}</td>
                            <td className="muted">{session.Machine}</td>
                            <td className="muted">{session.GitBranch}</td>
                            <td className="num">
                              {formatCount(session.MessageCount)}
                            </td>
                            <td className="num">
                              <HoverTip
                                summary={formatCount(sessionTokens(session))}
                                className="tok-cell"
                              >
                                <TokenCard
                                  input={session.TotalInput}
                                  output={session.TotalOutput}
                                  cacheRead={session.TotalCacheRead}
                                  cacheWrite={session.TotalCacheWrite}
                                  costUSD={session.TotalCostUSD}
                                  costIncomplete={session.CostIncomplete}
                                />
                              </HoverTip>
                            </td>
                            <td className="num">
                              {formatCost(
                                session.TotalCostUSD,
                                session.CostIncomplete,
                              )}
                            </td>
                            <td
                              className="muted"
                              title={formatTime(session.LastActiveAt)}
                            >
                              {relativeTime(session.LastActiveAt)}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                      {data.remainder.Sessions > 0 ? (
                        <tfoot>
                          <tr
                            className="remainder"
                            title="Older sessions in this range, beyond the rows shown"
                          >
                            <td colSpan={6}>
                              +{data.remainder.Sessions} more session
                              {data.remainder.Sessions === 1 ? "" : "s"} in this
                              range
                            </td>
                            <td className="num">
                              <HoverTip
                                summary={formatCount(remainderTokens)}
                                className="tok-cell"
                              >
                                <TokenCard
                                  input={data.remainder.Input}
                                  output={data.remainder.Output}
                                  cacheRead={data.remainder.CacheRead}
                                  cacheWrite={data.remainder.CacheWrite}
                                  costUSD={data.remainder.CostUSD}
                                  costIncomplete={data.remainder.CostIncomplete}
                                />
                              </HoverTip>
                            </td>
                            <td className="num">
                              {formatCost(
                                data.remainder.CostUSD,
                                data.remainder.CostIncomplete,
                              )}
                            </td>
                            <td></td>
                          </tr>
                        </tfoot>
                      ) : null}
                    </table>
                  </div>
                )}
              </section>
              <div className="project-insights">
                <h2>Quality signals</h2>
                <InsightsPanel insights={data.insights} />
                <TooltipHost>
                  <ToolsInstrument
                    insights={data.insights}
                    resetKey={`${data.project.ID}:${data.range}`}
                  />
                </TooltipHost>
              </div>
            </>
          );
        }}
      </AsyncView>
    </div>
  );
}
