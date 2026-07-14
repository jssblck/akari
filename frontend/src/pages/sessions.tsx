// The cross-project sessions feed: search, filter, and browse every session
// without first picking a project. Ported from sessions.templ / feed.go. The
// session detail page and the shared Transcript component live in their own
// files (pages/session-detail.tsx, components/transcript.tsx); this file
// re-exports them so main.tsx and public.tsx keep importing from one module.
import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { stripPromptPreamble } from "../components/session-quality";
import {
  FallbackTag,
  FanoutTag,
  GradeTag,
  KindTag,
  OutcomeTag,
  SessionPublicTag,
} from "../components/session-tags";
import { HoverTip, TokenCard } from "../components/token-card";
import { formatCount, formatTokens, relativeTime } from "../format";
import "../sessions.css";
import type { SessionRow } from "../types";

export { Transcript } from "../components/transcript";
export { SessionPage } from "./session-detail";

type FacetCount = { Value: string; Count: number };
type ProjectFacet = {
  ID: number;
  Key: string;
  Name: string;
  Kind: string;
  Count: number;
};
type SessionsResponse = {
  sessions: SessionRow[] | null;
  has_more: boolean;
  facets: {
    Agents: FacetCount[] | null;
    Machines: FacetCount[] | null;
    Users: FacetCount[] | null;
    Projects: ProjectFacet[] | null;
  };
};

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

// tokenPct mirrors web.tokenPct: a square-root scale so mid-range sessions
// differentiate on the bar rather than every row but the outlier pegging to a
// sliver.
function tokenPct(tok: number, max: number): number {
  if (max <= 0 || tok <= 0) return 0;
  const pct = Math.round(Math.sqrt(tok / max) * 100);
  return Math.min(100, Math.max(pct > 0 ? Math.max(pct, 3) : 0, 0));
}

function rowTokens(row: SessionRow): number {
  return (
    row.TotalInput + row.TotalOutput + row.TotalCacheRead + row.TotalCacheWrite
  );
}

type DayGroup = {
  label: string;
  rows: Array<{ row: SessionRow; fadeProject: boolean; tokenPct: number }>;
};

function buildFeed(
  rows: SessionRow[],
  grouped: boolean,
  maxTok: number,
): DayGroup[] {
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
      tokenPct: tokenPct(rowTokens(row), maxTok),
    });
    prevProj = proj;
  }
  return groups;
}

export function SessionsPage() {
  const [params, setParams] = useSearchParams();
  const [search, setSearch] = useState(params.get("q") ?? "");
  const filterKey = params.toString();
  const state = useAPI<SessionsResponse>(`/api/v1/app/sessions?${filterKey}`);

  const [morePages, setMorePages] = useState<SessionRow[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset keys off the filter query alone; params is a fresh object every render.
  useEffect(() => {
    setMorePages([]);
    setSearch(params.get("q") ?? "");
  }, [filterKey]);
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
  // The token bar's denominator is fixed at the first page's maximum (see
  // web.FeedMaxTokens): recomputing it per appended page would make a bar's
  // width incomparable across a "Show more" boundary.
  const maxTok = useMemo(
    () =>
      state.kind === "ready"
        ? Math.max(0, ...(state.data.sessions ?? []).map(rowTokens))
        : 0,
    [state],
  );
  const groups = useMemo(
    () => buildFeed(rows, grouped, maxTok),
    [rows, grouped, maxTok],
  );

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
      <header className="page-head">
        <div>
          <h1>Sessions</h1>
          <p>
            Search the full transcript corpus, then narrow by project or
            execution context.
          </p>
        </div>
      </header>
      <form
        className="search-bar"
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
          placeholder="Search messages"
        />
        <button className="button" type="submit">
          Search
        </button>
      </form>
      <AsyncView state={state}>
        {(data) => (
          <div className="session-browser">
            <aside className="filter-rail">
              <FacetGroup
                title="Projects"
                rows={(data.facets.Projects ?? []).map((item) => ({
                  value: String(item.ID),
                  label: projectFacetLabel(item),
                  count: item.Count,
                }))}
                active={params.get("project") ?? ""}
                onSelect={(value) =>
                  setParams(
                    setQuery(setQuery(params, "project", value), "after", ""),
                  )
                }
              />
              <FacetGroup
                title="Agents"
                rows={(data.facets.Agents ?? []).map((item) => ({
                  value: item.Value,
                  label: item.Value,
                  count: item.Count,
                }))}
                active={params.get("agent") ?? ""}
                onSelect={(value) =>
                  setParams(
                    setQuery(setQuery(params, "agent", value), "after", ""),
                  )
                }
              />
              {(data.facets.Machines ?? []).length > 1 ? (
                <FacetGroup
                  title="Machines"
                  rows={(data.facets.Machines ?? []).map((item) => ({
                    value: item.Value,
                    label: item.Value,
                    count: item.Count,
                  }))}
                  active={params.get("machine") ?? ""}
                  onSelect={(value) =>
                    setParams(
                      setQuery(setQuery(params, "machine", value), "after", ""),
                    )
                  }
                />
              ) : null}
              {(data.facets.Users ?? []).length > 1 ? (
                <FacetGroup
                  title="Accounts"
                  rows={(data.facets.Users ?? []).map((item) => ({
                    value: item.Value,
                    label: item.Value,
                    count: item.Count,
                  }))}
                  active={params.get("user") ?? ""}
                  onSelect={(value) =>
                    setParams(
                      setQuery(setQuery(params, "user", value), "after", ""),
                    )
                  }
                />
              ) : null}
              <section className="facet">
                <h2 className="label">Sort</h2>
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
              </section>
              <label className="check-row">
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
                />{" "}
                Include subagents
              </label>
              <label className="check-row">
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
                />{" "}
                Include empty
              </label>
            </aside>
            <section className="feed">
              <ActiveFilterChips
                params={params}
                projects={data.facets.Projects ?? []}
                setParams={setParams}
              />
              {(rows ?? []).length === 0 ? (
                <div className="empty-state">
                  <h2>No matching sessions</h2>
                  <p>Clear a filter or search for a different phrase.</p>
                </div>
              ) : (
                groups.map((group, gi) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: groups are rebuilt from the accumulated rows every render; the label alone is not unique (two undated groups never occur, but a stable index is simpler than deriving one).
                  <div key={gi}>
                    {group.label ? (
                      <div className="day-head">
                        <span className="day-label label">{group.label}</span>
                        <span className="day-rule" />
                      </div>
                    ) : null}
                    {group.rows.map((fr) => (
                      <SessionFeedRow
                        key={fr.row.ID}
                        session={fr.row}
                        fadeProject={fr.fadeProject}
                        tokenPct={fr.tokenPct}
                      />
                    ))}
                  </div>
                ))
              )}
              <div className="feed-footer">
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
                    {loadingMore ? "Loading…" : "Show more"}
                  </button>
                ) : null}
              </div>
            </section>
          </div>
        )}
      </AsyncView>
    </div>
  );
}

function FacetGroup({
  title,
  rows,
  active,
  onSelect,
}: {
  title: string;
  rows: Array<{ value: string; label: string; count: number }>;
  active: string;
  onSelect: (value: string) => void;
}) {
  if (rows.length === 0) return null;
  return (
    <section className="facet">
      <h2 className="label">{title}</h2>
      {active ? (
        <button
          type="button"
          className="facet-row active"
          onClick={() => onSelect("")}
        >
          <span>All</span>
          <span>clear</span>
        </button>
      ) : null}
      {rows.map((row) => (
        <button
          type="button"
          className={row.value === active ? "facet-row active" : "facet-row"}
          onClick={() => onSelect(row.value === active ? "" : row.value)}
          key={row.value}
        >
          <span>{row.label}</span>
          <span>{formatCount(row.count)}</span>
        </button>
      ))}
    </section>
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

function SessionFeedRow({
  session,
  fadeProject,
  tokenPct: pct,
}: {
  session: SessionRow;
  fadeProject: boolean;
  tokenPct: number;
}) {
  const tokens = rowTokens(session);
  const title = stripPromptPreamble(session.Title);
  return (
    <Link to={`/sessions/${session.ID}`} className="srow">
      <span className="srow-agent mono small">{session.Agent}</span>
      <div className="srow-main">
        <div className="srow-line">
          <span className={fadeProject ? "srow-proj faded" : "srow-proj"}>
            {sessionRowProject(session)}
          </span>
          {session.GitBranch ? (
            <span className="srow-branch mono">{session.GitBranch}</span>
          ) : null}
          <KindTag kind={session.ProjectKind} />
          <GradeTag grade={session.Grade} />
          <OutcomeTag outcome={session.Outcome} />
          <SessionPublicTag
            visibility={session.Visibility}
            publicID={session.PublicID}
            linked={false}
          />
          <FallbackTag count={session.ModelFallbackCount} />
          <FanoutTag
            subagentCount={session.Tree.SubagentCount}
            costUSD={session.Tree.TotalCostUSD}
            costIncomplete={session.Tree.CostIncomplete}
          />
        </div>
        {session.Search.Text ? (
          <div className="srow-sub srow-snippet" title={session.Search.Text}>
            {session.Search.Text.slice(0, session.Search.MatchStart)}
            <mark>
              {session.Search.Text.slice(
                session.Search.MatchStart,
                session.Search.MatchEnd,
              )}
            </mark>
            {session.Search.Text.slice(session.Search.MatchEnd)}
          </div>
        ) : title ? (
          <div className="srow-sub" title={title}>
            {title}
          </div>
        ) : null}
      </div>
      <span className="srow-msgs mono small" title="messages">
        {session.MessageCount}
      </span>
      <HoverTip
        className="srow-tok"
        summary={
          <>
            <span className="tokbar">
              <span className="tokbar-fill" style={{ width: `${pct}%` }} />
            </span>
            <span className="tok-total mono">{formatTokens(tokens)}</span>
          </>
        }
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
      <span
        className="srow-time mono small"
        title={session.LastActiveAt ?? undefined}
      >
        {relativeTime(session.LastActiveAt)}
      </span>
    </Link>
  );
}
