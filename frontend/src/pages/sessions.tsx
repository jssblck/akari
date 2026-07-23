// The cross-project sessions feed: search, filter, and browse every session
// without first picking a project. Ported from sessions.templ / feed.go. The
// session detail page and the shared Transcript component live in their own
// files (pages/session-detail.tsx, components/transcript.tsx); this file
// re-exports them so main.tsx and public.tsx keep importing from one module.

import { FunnelIcon } from "@phosphor-icons/react";
import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { stripPromptPreamble } from "../components/session-quality";
import { SessionGrade, SessionOutcome } from "../components/session-signals";
import { FallbackTag, SessionPublicTag } from "../components/session-tags";
import { HoverTip, useHoverPopover } from "../components/token-card";
import {
  formatCost,
  formatCount,
  formatTokens,
  relativeTime,
  sessionTokens,
} from "../format";
import "../sessions.css";
import type { ProjectFacet, SessionRow, SessionsResponse } from "../types";

export { Transcript } from "../components/transcript";
export { SessionPage } from "./session-detail";

const SORT_OPTIONS = [
  { key: "updated", label: "Recent" },
  { key: "tokens", label: "Most tokens" },
  { key: "messages", label: "Most messages" },
  { key: "cost", label: "Most expensive" },
];

const GRADE_LABELS: Record<string, string> = {
  A: "A",
  B: "B",
  C: "C",
  D: "D",
  F: "F",
  unscored: "Unscored",
};

const OUTCOME_LABELS: Record<string, string> = {
  completed: "Completed",
  errored: "Errored",
  abandoned: "Abandoned",
  unknown: "Unknown",
};

function setQuery(
  params: URLSearchParams,
  key: string,
  value: string,
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (value) next.set(key, value);
  else next.delete(key);
  return next;
}

function isLocalKind(kind: string): boolean {
  return kind === "standalone" || kind === "orphaned";
}

function projectFacetLabel(pf: ProjectFacet): string {
  return isLocalKind(pf.Kind) ? pf.Name : pf.Key;
}

function sessionRowProject(row: SessionRow): string {
  return isLocalKind(row.ProjectKind) ? row.ProjectName : row.ProjectKey;
}

// keysetCursorValue mirrors web.keysetCursorValue: the exact, round-trippable
// text of the sort column's last visible value, carried into "Show more" so the
// next page resumes from what the reader already saw rather than a boundary
// that can drift under it (activity bumps last_active_at, a rebuild moves a
// count or cost).
export function keysetCursorValue(sort: string, row: SessionRow): string {
  switch (sort) {
    case "tokens":
      return String(
        row.TotalInput +
          row.TotalOutput +
          row.TotalCacheRead +
          row.TotalCacheWrite,
      );
    case "messages":
      return String(row.MessageCount);
    case "cost":
      return String(row.TotalCostUSD);
    default:
      return row.LastActiveAt ?? "";
  }
}

// dayBucket mirrors web.dayBucket: a stable grouping key (the viewer's local
// calendar date) and a relative display label, so the feed groups by day the
// same way the day-grouped server render did.
function dayBucket(
  now: Date,
  t: string | null,
): { key: string; label: string } {
  if (!t) return { key: "", label: "Undated" };
  const stamp = new Date(t);
  const nd = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const td = new Date(stamp.getFullYear(), stamp.getMonth(), stamp.getDate());
  const key = td.toISOString().slice(0, 10);
  const days = Math.round((nd.getTime() - td.getTime()) / 86_400_000);
  if (days <= 0) return { key, label: "Today" };
  if (days === 1) return { key, label: "Yesterday" };
  if (days < 7)
    return {
      key,
      label: stamp.toLocaleDateString(undefined, { weekday: "long" }),
    };
  if (nd.getFullYear() === td.getFullYear())
    return {
      key,
      label: stamp.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
      }),
    };
  return {
    key,
    label: stamp.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    }),
  };
}

type DayGroup = {
  label: string;
  rows: Array<{ row: SessionRow; fadeProject: boolean }>;
};

function buildFeed(rows: SessionRow[], grouped: boolean): DayGroup[] {
  if (rows.length === 0) return [];
  const now = new Date();
  const groups: DayGroup[] = [];
  let curKey: string | null = null;
  let prevProj = "";
  for (const row of rows) {
    if (grouped) {
      const { key, label } = dayBucket(now, row.LastActiveAt);
      if (curKey === null || key !== curKey) {
        groups.push({ label, rows: [] });
        curKey = key;
        prevProj = "";
      }
    } else if (groups.length === 0) {
      groups.push({ label: "", rows: [] });
    }
    const proj = sessionRowProject(row);
    const group = groups[groups.length - 1];
    if (!group) continue;
    group.rows.push({
      row,
      fadeProject: proj === prevProj,
    });
    prevProj = proj;
  }
  return groups;
}

export function SessionsPage() {
  const [params, setParams] = useSearchParams();
  const [search, setSearch] = useState(params.get("q") ?? "");
  const committedSearch = params.get("q") ?? "";
  const filterKey = params.toString();
  const state = useAPI<SessionsResponse>(`/api/v1/app/sessions?${filterKey}`);

  const [morePages, setMorePages] = useState<SessionRow[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [filtersOpen, setFiltersOpen] = useState(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: filterKey deliberately resets pagination; the effect does not otherwise read it.
  useEffect(() => {
    setMorePages([]);
  }, [filterKey]);
  useEffect(() => setSearch(committedSearch), [committedSearch]);
  useEffect(() => {
    const nextSearch = search.trim();
    if (nextSearch === committedSearch) return;
    const timeout = window.setTimeout(() => {
      setParams(setQuery(setQuery(params, "q", nextSearch), "after", ""), {
        replace: true,
      });
    }, 250);
    return () => window.clearTimeout(timeout);
  }, [committedSearch, params, search, setParams]);
  useEffect(() => {
    if (state.kind === "ready") setHasMore(state.data.has_more);
  }, [state]);

  const sort = params.get("sort") || "updated";
  const grouped = sort === "updated";

  const rows = useMemo(
    () => [
      ...(state.kind === "ready" ? (state.data.sessions ?? []) : []),
      ...morePages,
    ],
    [state, morePages],
  );
  const groups = useMemo(() => buildFeed(rows, grouped), [rows, grouped]);

  async function loadMore() {
    const last = rows[rows.length - 1];
    if (!last) return;
    setLoadingMore(true);
    try {
      const next = setQuery(
        setQuery(params, "after", String(last.ID)),
        "after_value",
        keysetCursorValue(sort, last),
      );
      const result = await request<SessionsResponse>(
        `/api/v1/app/sessions?${next.toString()}`,
      );
      setMorePages((cur) => [...cur, ...(result.sessions ?? [])]);
      setHasMore(result.has_more);
    } finally {
      setLoadingMore(false);
    }
  }

  return (
    <div className="page sessions-page">
      <form
        className="search-bar sessions-search"
        onSubmit={(event) => {
          event.preventDefault();
          setParams(
            setQuery(setQuery(params, "q", search.trim()), "after", ""),
          );
        }}
      >
        <input
          aria-label="Search session content"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="Search sessions"
        />
      </form>
      <AsyncView state={state}>
        {(data) => (
          <section className="sessions-list">
            <header
              className={
                filtersOpen
                  ? "sessions-toolbar filters-open"
                  : "sessions-toolbar"
              }
            >
              <button
                type="button"
                className="filters-toggle"
                aria-expanded={filtersOpen}
                aria-controls="sessions-scope-panel sessions-filter-panel"
                onClick={() => setFiltersOpen((open) => !open)}
              >
                <FunnelIcon size={14} aria-hidden="true" />
                Filters
              </button>
              <div className="sessions-scope" id="sessions-scope-panel">
                <span className="sessions-total">
                  Sessions
                  <span className="count-badge">
                    {hasMore ? `${rows.length}+` : rows.length}
                  </span>
                </span>
                <label className="sessions-toggle">
                  <input
                    type="checkbox"
                    checked={params.get("subagents") === "1"}
                    onChange={(event) =>
                      setParams(
                        setQuery(
                          setQuery(
                            params,
                            "subagents",
                            event.target.checked ? "1" : "",
                          ),
                          "after",
                          "",
                        ),
                      )
                    }
                  />
                  Subagents
                </label>
                <label className="sessions-toggle">
                  <input
                    type="checkbox"
                    checked={params.get("empty") === "1"}
                    onChange={(event) =>
                      setParams(
                        setQuery(
                          setQuery(
                            params,
                            "empty",
                            event.target.checked ? "1" : "",
                          ),
                          "after",
                          "",
                        ),
                      )
                    }
                  />
                  Empty
                </label>
              </div>
              <div
                className="sessions-filter-controls"
                id="sessions-filter-panel"
              >
                <select
                  aria-label="Project"
                  value={params.get("project") ?? ""}
                  onChange={(event) =>
                    setParams(
                      setQuery(
                        setQuery(params, "project", event.target.value),
                        "after",
                        "",
                      ),
                    )
                  }
                >
                  <option value="">All projects</option>
                  {(data.facets.Projects ?? []).map((item) => (
                    <option key={item.ID} value={String(item.ID)}>
                      {projectFacetLabel(item)} ({formatCount(item.Count)})
                    </option>
                  ))}
                </select>
                <select
                  aria-label="Agent"
                  value={params.get("agent") ?? ""}
                  onChange={(event) =>
                    setParams(
                      setQuery(
                        setQuery(params, "agent", event.target.value),
                        "after",
                        "",
                      ),
                    )
                  }
                >
                  <option value="">All agents</option>
                  {(data.facets.Agents ?? []).map((item) => (
                    <option key={item.Value} value={item.Value}>
                      {item.Value} ({formatCount(item.Count)})
                    </option>
                  ))}
                </select>
                {(data.facets.Machines ?? []).length > 1 ? (
                  <select
                    aria-label="Machine"
                    value={params.get("machine") ?? ""}
                    onChange={(event) =>
                      setParams(
                        setQuery(
                          setQuery(params, "machine", event.target.value),
                          "after",
                          "",
                        ),
                      )
                    }
                  >
                    <option value="">All machines</option>
                    {(data.facets.Machines ?? []).map((item) => (
                      <option key={item.Value} value={item.Value}>
                        {item.Value} ({formatCount(item.Count)})
                      </option>
                    ))}
                  </select>
                ) : null}
                {(data.facets.Users ?? []).length > 1 ? (
                  <select
                    aria-label="Account"
                    value={params.get("user") ?? ""}
                    onChange={(event) =>
                      setParams(
                        setQuery(
                          setQuery(params, "user", event.target.value),
                          "after",
                          "",
                        ),
                      )
                    }
                  >
                    <option value="">All accounts</option>
                    {(data.facets.Users ?? []).map((item) => (
                      <option key={item.Value} value={item.Value}>
                        {item.Value} ({formatCount(item.Count)})
                      </option>
                    ))}
                  </select>
                ) : null}
                <select
                  aria-label="Sort"
                  value={sort}
                  onChange={(event) =>
                    setParams(
                      setQuery(
                        setQuery(params, "sort", event.target.value),
                        "after",
                        "",
                      ),
                    )
                  }
                >
                  {SORT_OPTIONS.map((opt) => (
                    <option key={opt.key} value={opt.key}>
                      {opt.label}
                    </option>
                  ))}
                </select>
              </div>
            </header>
            <ActiveFilterChips
              params={params}
              projects={data.facets.Projects ?? []}
              setParams={setParams}
            />
            <div className="sessions-feed">
              {(rows ?? []).length === 0 ? (
                <div className="empty-state">
                  <h2>No matching sessions</h2>
                  <p>Clear a filter or search for a different phrase.</p>
                </div>
              ) : (
                groups.map((group, gi) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: groups are rebuilt from the accumulated rows every render; the label alone is not unique (two undated groups never occur, but a stable index is simpler than deriving one).
                  <div className="session-day" key={gi}>
                    {group.label ? (
                      <div className="day-head">
                        <span className="day-label">{group.label}</span>
                      </div>
                    ) : null}
                    {group.rows.map((fr) => (
                      <SessionFeedRow
                        key={fr.row.ID}
                        session={fr.row}
                        fadeProject={fr.fadeProject}
                      />
                    ))}
                  </div>
                ))
              )}
            </div>
            <footer className="feed-footer">
              <span className="feed-count mono small">
                {hasMore
                  ? `Showing ${rows.length}`
                  : rows.length === 1
                    ? "1 session"
                    : `${rows.length} sessions`}
              </span>
              {hasMore ? (
                <button
                  type="button"
                  className="small showmore"
                  disabled={loadingMore}
                  onClick={loadMore}
                >
                  {loadingMore ? "Loading..." : "Show more"}
                </button>
              ) : null}
            </footer>
          </section>
        )}
      </AsyncView>
    </div>
  );
}

// ActiveFilterChips reads every filter the URL may carry (including grade,
// outcome, and range, which arrive only from an Insights drill-through link;
// the toolbar has no picker for them, but a reader who followed a link here
// still needs to see and clear what narrowed the feed).
function ActiveFilterChips({
  params,
  projects,
  setParams,
}: {
  params: URLSearchParams;
  projects: ProjectFacet[];
  setParams: (params: URLSearchParams) => void;
}) {
  const chips: Array<{ key: string; label: string; value: string }> = [];
  const agent = params.get("agent");
  if (agent) chips.push({ key: "agent", label: "agent", value: agent });
  const machine = params.get("machine");
  if (machine) chips.push({ key: "machine", label: "machine", value: machine });
  const user = params.get("user");
  if (user) chips.push({ key: "user", label: "user", value: user });
  const projectID = params.get("project");
  if (projectID) {
    const match = projects.find((p) => String(p.ID) === projectID);
    chips.push({
      key: "project",
      label: "project",
      value: match ? projectFacetLabel(match) : projectID,
    });
  }
  const q = params.get("q");
  if (q) chips.push({ key: "q", label: "search", value: q });
  const grade = params.get("grade");
  if (grade)
    chips.push({
      key: "grade",
      label: "grade",
      value: GRADE_LABELS[grade] ?? grade,
    });
  const outcome = params.get("outcome");
  if (outcome)
    chips.push({
      key: "outcome",
      label: "outcome",
      value: OUTCOME_LABELS[outcome] ?? outcome,
    });
  const range = params.get("range");
  if (range) chips.push({ key: "range", label: "range", value: range });

  if (chips.length === 0) return null;
  return (
    <div className="active-filters">
      {chips.map((chip) => (
        <button
          type="button"
          key={chip.key}
          className="fchip"
          onClick={() =>
            setParams(setQuery(setQuery(params, chip.key, ""), "after", ""))
          }
        >
          <span className="fchip-k">{chip.label}</span>
          <span>{chip.value}</span>
          <span className="fchip-x">&times;</span>
        </button>
      ))}
      <button
        type="button"
        className="small clear"
        onClick={() => setParams(new URLSearchParams())}
      >
        Clear all
      </button>
    </div>
  );
}

// splitTitleTail separates a title into a head and a fixed-length tail
// rendered as two flex children (see .srow-title in sessions.css): the head
// gets a CSS ellipsis and the tail never shrinks. A JS-computed head length
// can only ever assume one column width, and the row is exactly as wide as a
// phone screen as it is a desktop one, so letting the browser's own ellipsis
// size the head means the same split works at any width, and the part that
// tells two automation runs apart (an "Automation ID: ..." suffix, say)
// survives instead of being the part an end truncation cuts.
export function splitTitleTail(
  text: string,
  tailLen = 24,
): { head: string; tail: string } {
  const collapsed = text.replace(/\s+/g, " ").trim();
  if (collapsed.length <= tailLen) return { head: collapsed, tail: "" };
  // Start the tail at a word boundary. The seam only becomes visible once the
  // head ellipsizes, which is exactly when the tail earns its keep, and a tail
  // that opens mid-word there ("...ush current state please") reads as a
  // rendering fault rather than as the end of the prompt. The space belongs to
  // the tail, whose white-space: pre keeps it, so head + tail still reproduces
  // the collapsed title exactly on a row wide enough to show all of it. A final
  // token longer than the tail (a hash, a URL) has no boundary to snap to and
  // keeps the hard cut, which is the right answer for an id.
  const hardCut = collapsed.length - tailLen;
  const boundary = collapsed.indexOf(" ", hardCut);
  const cut = boundary === -1 ? hardCut : boundary;
  return { head: collapsed.slice(0, cut), tail: collapsed.slice(cut) };
}

function SessionFeedRow({
  session,
  fadeProject,
}: {
  session: SessionRow;
  fadeProject: boolean;
}) {
  const title =
    stripPromptPreamble(session.Title) || sessionRowProject(session);
  const { head, tail } = splitTitleTail(title);
  const {
    triggerRef: rowRef,
    popoverRef: overviewRef,
    show: showOverview,
    hide: hideOverview,
  } = useHoverPopover<HTMLAnchorElement, HTMLSpanElement>();

  return (
    <Link
      to={`/sessions/${session.ID}`}
      className="srow"
      ref={rowRef}
      onFocus={(event) => {
        if (event.currentTarget === event.target) showOverview();
      }}
      onBlur={hideOverview}
      onMouseOver={(event) => {
        const target = event.target as HTMLElement;
        if (target.closest?.(".hover-tip")) hideOverview();
        else showOverview(event);
      }}
      onMouseLeave={hideOverview}
    >
      <div className="srow-main">
        <div className="srow-line">
          {session.Search.Text ? (
            <span className="srow-title srow-snippet">
              {session.Search.Text.slice(0, session.Search.MatchStart)}
              <mark>
                {session.Search.Text.slice(
                  session.Search.MatchStart,
                  session.Search.MatchEnd,
                )}
              </mark>
              {session.Search.Text.slice(session.Search.MatchEnd)}
            </span>
          ) : (
            <span className="srow-title">
              <span className="srow-title-head">{head}</span>
              {tail && <span className="srow-title-tail">{tail}</span>}
            </span>
          )}
          <FallbackTag count={session.ModelFallbackCount} />
        </div>
        <div className="srow-meta">
          <span className={fadeProject ? "srow-proj faded" : "srow-proj"}>
            {sessionRowProject(session)}
          </span>
          <span className="srow-agent">{session.Agent}</span>
          {session.GitBranch ? (
            <span className="srow-branch mono">{session.GitBranch}</span>
          ) : null}
          <ActivityDate value={session.LastActiveAt} />
        </div>
      </div>
      <div className="srow-signals">
        <ProjectKindCell kind={session.ProjectKind} />
        <FanoutCell session={session} />
        <PublicCell session={session} />
        <span className="srow-cost mono">
          {formatCost(session.TotalCostUSD)}
        </span>
        <SessionGrade grade={session.Grade} />
        <SessionOutcome outcome={session.Outcome} endedAt={session.EndedAt} />
      </div>
      <span
        className="session-overview popover"
        role="tooltip"
        popover="manual"
        ref={overviewRef}
      >
        <strong className="tip-title">Session overview</strong>
        <dl className="session-overview-grid">
          <dt>Messages</dt>
          <dd>{formatCount(session.MessageCount)}</dd>
          <dt>User prompts</dt>
          <dd>{formatCount(session.UserMessageCount)}</dd>
          <dt>Total tokens</dt>
          <dd>{formatTokens(sessionTokens(session))}</dd>
          <dt>Session cost</dt>
          <dd>{formatCost(session.TotalCostUSD)}</dd>
        </dl>
        <span className="session-overview-meta">
          {session.Agent} on {session.Machine}
        </span>
      </span>
    </Link>
  );
}

function ActivityDate({ value }: { value: string | null }) {
  const fullDate = value
    ? new Intl.DateTimeFormat(undefined, {
        dateStyle: "full",
        timeStyle: "short",
      }).format(new Date(value))
    : "No activity timestamp is available.";
  return (
    <HoverTip
      className="srow-date"
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

function ProjectKindCell({ kind }: { kind: string }) {
  if (kind !== "standalone" && kind !== "orphaned")
    return <span className="srow-kind-empty" />;
  const detail =
    kind === "standalone"
      ? "No Git repository or origin remote was found for this session."
      : "The working directory no longer exists on disk.";
  return (
    <HoverTip
      className="srow-signal srow-kind"
      summary={<span className={`tag ${kind}`}>{kind}</span>}
    >
      <strong className="tip-title">{kind}</strong>
      <p className="tip-copy">{detail}</p>
    </HoverTip>
  );
}

// PublicCell holds the same fixed-width slot as the kind and fan-out chips
// (an empty placeholder when the row is private) so a public session's badge
// never needs a line of its own the way it did sitting inline with the title.
function PublicCell({ session }: { session: SessionRow }) {
  if (session.Visibility !== "public")
    return <span className="srow-public-empty" />;
  return (
    <span className="srow-public">
      <SessionPublicTag
        visibility={session.Visibility}
        publicID={session.PublicID}
        linked={false}
      />
    </span>
  );
}

function FanoutCell({ session }: { session: SessionRow }) {
  const count = session.Tree.SubagentCount;
  if (count <= 0) return <span className="srow-fanout-empty" />;
  const unit = count === 1 ? "subagent" : "subagents";
  return (
    <HoverTip
      className="srow-signal srow-fanout"
      summary={
        <span className="tag fanout">
          {count} {unit}
        </span>
      }
    >
      <strong className="tip-title">Whole work item</strong>
      <dl className="tt-grid">
        <dt>Subagents</dt>
        <dd>{formatCount(count)}</dd>
        <dt>Root session</dt>
        <dd>{formatCost(session.TotalCostUSD)}</dd>
        <dt>Total cost</dt>
        <dd>{formatCost(session.Tree.CostUSD)}</dd>
      </dl>
      <p className="tip-copy">
        Total cost includes the root session and its subagents.
      </p>
    </HoverTip>
  );
}
