import { ArrowSquareOutIcon } from "@phosphor-icons/react";
import { type MouseEvent, useState } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";

import { useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { ActivityBars } from "../components/charts";
import { ToolsInstrument } from "../components/insights/tools";
import { TooltipHost } from "../components/insights/tooltip";
import { RangeTabs } from "../components/range-tabs";
import { stripPromptPreamble } from "../components/session-quality";
import { SessionGrade, SessionOutcome } from "../components/session-signals";
import { HoverTip, TokenCard } from "../components/token-card";
import {
  formatCost,
  formatCount,
  formatTime,
  relativeTime,
  sessionTokens,
} from "../format";
import "../projects.css";
import { withBase } from "../base";
import { normalizeInsights } from "../normalize-insights";
import type {
  Project,
  ProjectResponse,
  ProjectsResponse,
  SessionSummary,
} from "../types";
import { InsightsPanel } from "./insights";
import { AnalyticsPanel } from "./overview";

// isLocalKind mirrors the server's IsLocalKind: a standalone or orphaned
// project has no git remote, so it groups and labels apart from a repository
// in the projects ledger and session filter facets.
function isLocalKind(kind: string): boolean {
  return kind === "standalone" || kind === "orphaned";
}

function normalizeSparklines(
  sparklines: ProjectsResponse["sparklines"],
): Record<string, number[]> {
  return Object.fromEntries(
    Object.entries(sparklines ?? {}).map(([key, values]) => [
      key,
      values ?? [],
    ]),
  );
}

type ProjectSort = "recent" | "sessions" | "tokens" | "cost";

const PROJECT_SORTS: Array<{ value: ProjectSort; label: string }> = [
  { value: "recent", label: "Recent" },
  { value: "sessions", label: "Most sessions" },
  { value: "tokens", label: "Most tokens" },
  { value: "cost", label: "Highest cost" },
];

function parseProjectSort(value: string): ProjectSort {
  switch (value) {
    case "sessions":
    case "tokens":
    case "cost":
      return value;
    default:
      return "recent";
  }
}

function projectTokens(project: Project): number {
  return (
    project.TotalInput +
    project.TotalOutput +
    project.TotalCacheRead +
    project.TotalCacheWrite
  );
}

function projectTimestamp(project: Project): number {
  return project.LastActivity ? new Date(project.LastActivity).getTime() : 0;
}

function orderProjects(projects: Project[], sort: ProjectSort): Project[] {
  return [...projects].sort((left, right) => {
    let difference = projectTimestamp(right) - projectTimestamp(left);
    if (sort === "sessions")
      difference = right.SessionCount - left.SessionCount;
    if (sort === "tokens")
      difference = projectTokens(right) - projectTokens(left);
    if (sort === "cost") difference = right.TotalCostUSD - left.TotalCostUSD;
    return difference || projectTimestamp(right) - projectTimestamp(left);
  });
}

function matchingProjects(
  projects: Project[],
  query: string,
  sort: ProjectSort,
): Project[] {
  const needle = query.trim().toLocaleLowerCase();
  const matches = needle
    ? projects.filter((project) =>
        [
          project.DisplayName,
          project.RemoteKey,
          project.Host,
          localPath(project),
        ].some((value) => value.toLocaleLowerCase().includes(needle)),
      )
    : projects;
  return orderProjects(matches, sort);
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

function ProjectKindCell({ kind }: { kind: string }) {
  if (kind !== "standalone" && kind !== "orphaned")
    return <span className="project-kind-empty" />;
  const detail =
    kind === "standalone"
      ? "No Git repository or origin remote was found for this project."
      : "The working directory no longer exists on disk.";
  return (
    <HoverTip
      className="project-kind"
      summary={<span className={`tag ${kind}`}>{kind}</span>}
    >
      <strong className="tip-title">{kind}</strong>
      <p className="tip-copy">{detail}</p>
    </HoverTip>
  );
}

function ProjectTokensCell({ project }: { project: Project }) {
  return (
    <HoverTip
      summary={
        <span className="project-token-total">
          {formatCount(projectTokens(project))}
        </span>
      }
      className="tok-cell"
    >
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

function ProjectActivityCell({ values }: { values: number[] | undefined }) {
  const buckets = values ?? [];
  const activeDays = buckets.filter((value) => value > 0).length;
  const total = buckets.reduce((sum, value) => sum + Math.max(value, 0), 0);
  const peak = Math.max(...buckets, 0);
  return (
    <HoverTip
      className="project-activity"
      summary={<ActivityBars values={values} />}
    >
      <strong className="tip-title">30-day token activity</strong>
      <dl className="tt-grid">
        <dt>Active days</dt>
        <dd>{formatCount(activeDays)}</dd>
        <dt>Total tokens</dt>
        <dd>{formatCount(total)}</dd>
        <dt>Peak day</dt>
        <dd>{formatCount(peak)}</dd>
      </dl>
    </HoverTip>
  );
}

function ProjectDateCell({ value }: { value: string | null }) {
  const fullDate = value
    ? new Intl.DateTimeFormat(undefined, {
        dateStyle: "full",
        timeStyle: "short",
      }).format(new Date(value))
    : "No activity timestamp is available.";
  return (
    <HoverTip
      className="project-date"
      summary={<span>{relativeTime(value)}</span>}
    >
      <strong className="tip-title">Last active</strong>
      {value ? (
        <time className="tip-date" dateTime={value}>
          {fullDate}
        </time>
      ) : (
        <span className="tip-date">{fullDate}</span>
      )}
    </HoverTip>
  );
}

function ProjectLocation({ value, tail }: { value: string; tail: boolean }) {
  return (
    <HoverTip
      className="project-location-tip"
      summary={
        <span
          className={
            tail ? "project-location-path tail" : "project-location-path"
          }
        >
          {value}
        </span>
      }
    >
      <strong className="tip-title">Location</strong>
      <code className="project-location-full">{value}</code>
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
}: {
  project: Project;
  sparkline: number[] | undefined;
}) {
  const navigate = useNavigate();
  const local = isLocalKind(project.Kind);
  const handleRowClick = (event: MouseEvent<HTMLTableRowElement>) => {
    if (event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey)
      return;
    if ((event.target as HTMLElement).closest("a, .hover-tip")) return;
    navigate(`/projects/${project.ID}`);
  };
  return (
    <tr className="row-link" onClick={handleRowClick}>
      <td className="project-identity-cell">
        <Link className="primary-link" to={`/projects/${project.ID}`}>
          {local
            ? project.DisplayName || `Project ${project.ID}`
            : projectLabel(project)}
        </Link>
        <span className="cell-note project-location">
          <ProjectLocation
            value={local ? localPath(project) : project.RemoteKey}
            tail={local}
          />
          {local && project.Host ? (
            <>
              <span className="project-meta-separator" aria-hidden="true">
                /
              </span>
              <span className="project-location-host">{project.Host}</span>
            </>
          ) : null}
        </span>
      </td>
      <td className="project-kind-cell">
        <ProjectKindCell kind={project.Kind} />
      </td>
      <td className="num">{formatCount(project.SessionCount)}</td>
      <td className="num">
        <ProjectTokensCell project={project} />
      </td>
      <td className="project-activity-cell">
        <ProjectActivityCell values={sparkline} />
      </td>
      <td className="project-date-cell">
        <ProjectDateCell value={project.LastActivity} />
      </td>
    </tr>
  );
}

// ProjectSection keeps repositories and local folders in separate ledgers so
// an empty group disappears instead of leaving an empty table behind.
function ProjectSection({
  title,
  projects,
  totalProjects,
  sparklines,
  sort,
  onSortChange,
}: {
  title: string;
  projects: Project[];
  totalProjects: number;
  sparklines: Record<string, number[]>;
  sort: ProjectSort;
  onSortChange: (sort: ProjectSort) => void;
}) {
  if (projects.length === 0) return null;
  const countLabel = `${formatCount(projects.length)}${
    projects.length === totalProjects ? "" : ` of ${formatCount(totalProjects)}`
  } project${totalProjects === 1 ? "" : "s"}`;
  return (
    <section className="instrument compact proj-section">
      <div className="section-head">
        <div className="project-section-title">
          <h2>{title}</h2>
          <span className="count-badge" aria-hidden="true">
            {projects.length === totalProjects
              ? formatCount(totalProjects)
              : `${formatCount(projects.length)}/${formatCount(totalProjects)}`}
          </span>
          <span className="sr-only">{countLabel}</span>
        </div>
        <select
          className="project-sort"
          aria-label={`Sort ${title.toLocaleLowerCase()}`}
          value={sort}
          onChange={(event) =>
            onSortChange(parseProjectSort(event.target.value))
          }
        >
          {PROJECT_SORTS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </div>
      <div className="table-wrap embedded">
        <table className="data-table projects-table">
          <colgroup>
            <col className="project-col-name" />
            <col className="project-col-kind" />
            <col className="project-col-sessions" />
            <col className="project-col-tokens" />
            <col className="project-col-activity" />
            <col className="project-col-active" />
          </colgroup>
          <thead>
            <tr>
              <th>Project</th>
              <th>Kind</th>
              <th className="num">Sessions</th>
              <th className="num">Tokens</th>
              <th>30 day tokens</th>
              <th>Last active</th>
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => (
              <ProjectRow
                key={project.ID}
                project={project}
                sparkline={sparklines[String(project.ID)]}
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
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<ProjectSort>("recent");
  return (
    <div className="page projects-page">
      <div className="projects-toolbar">
        <input
          type="search"
          value={query}
          aria-label="Search projects"
          placeholder="Search projects"
          onChange={(event) => setQuery(event.target.value)}
        />
      </div>
      <AsyncView state={state}>
        {(data) => {
          const allProjects = data.projects ?? [];
          if (allProjects.length === 0)
            return (
              <section className="empty-state">
                <h2>No projects yet</h2>
                <p>
                  Run an akari client sync to create the first project and
                  session.
                </p>
                <a className="button" href={withBase("/guide")}>
                  Read the setup guide
                </a>
              </section>
            );
          const repositories = allProjects.filter(
            (project) => !isLocalKind(project.Kind),
          );
          const localFolders = allProjects.filter((project) =>
            isLocalKind(project.Kind),
          );
          const visibleRepositories = matchingProjects(
            repositories,
            query,
            sort,
          );
          const visibleLocalFolders = matchingProjects(
            localFolders,
            query,
            sort,
          );
          if (
            visibleRepositories.length === 0 &&
            visibleLocalFolders.length === 0
          )
            return (
              <section className="empty-state projects-empty">
                <h2>No matching projects</h2>
                <p>Search for a different name, path, remote, or host.</p>
              </section>
            );
          const sparklines = normalizeSparklines(data.sparklines);
          return (
            <>
              <ProjectSection
                title="Repositories"
                projects={visibleRepositories}
                totalProjects={repositories.length}
                sparklines={sparklines}
                sort={sort}
                onSortChange={setSort}
              />
              <ProjectSection
                title="Local folders"
                projects={visibleLocalFolders}
                totalProjects={localFolders.length}
                sparklines={sparklines}
                sort={sort}
                onSortChange={setSort}
              />
            </>
          );
        }}
      </AsyncView>
    </div>
  );
}

// ProjectToolbar is the project page's session filter: three auto-applying
// selects (Agent, User, Machine) that narrow the whole scoped view (the usage
// panel and the session table both re-fetch under the chosen facets), reading
// and writing the same URL params the project API already accepts. It renders
// beside the activity range because those controls describe the same scope.
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
        href={withBase(`/s/${session.PublicID}`)}
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
    <div className="page project-page">
      <AsyncView state={state}>
        {(data) => {
          const insights = normalizeInsights(data.insights);
          const remainderTokens =
            data.remainder.Input +
            data.remainder.Output +
            data.remainder.CacheRead +
            data.remainder.CacheWrite;
          return (
            <>
              <header className="page-head">
                <span className="crumb">
                  <Link to="/projects">Projects</Link> /{" "}
                  {projectLabel(data.project)}
                </span>
              </header>
              <AnalyticsPanel
                analytics={data.analytics}
                showUsers
                mobileActivity="range-only"
                activityControls={
                  <>
                    <ProjectToolbar facets={data.facets} />
                    <RangeTabs ranges={data.ranges ?? []} active={data.range} />
                  </>
                }
              />
              <section className="instrument compact project-sessions">
                <div className="section-head">
                  <h2>Sessions</h2>
                </div>
                {(data.sessions ?? []).length === 0 ? (
                  <p className="empty-inline">
                    No sessions match these filters.
                  </p>
                ) : (
                  <div className="table-wrap embedded">
                    <table className="data-table project-sessions-table">
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
                          <th className="project-session-signal-head">
                            <span className="sr-only">Signals</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {(data.sessions ?? []).map((session) => (
                          <tr key={session.ID}>
                            <td className="project-session-title-cell">
                              <Link
                                className="primary-link project-session-title"
                                to={`/sessions/${session.ID}`}
                              >
                                {stripPromptPreamble(session.Title) ||
                                  `Session ${session.ID}`}
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
                            <td className="project-session-signals">
                              <SessionGrade grade={session.Grade} />
                              <SessionOutcome outcome={session.Outcome} />
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
                            <td className="project-session-signals"></td>
                          </tr>
                        </tfoot>
                      ) : null}
                    </table>
                  </div>
                )}
              </section>
              <div className="project-insights">
                <h2>Quality signals</h2>
                <InsightsPanel insights={insights} />
                <TooltipHost>
                  <ToolsInstrument
                    insights={insights}
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
