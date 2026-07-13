import {
  ArrowSquareOutIcon,
  CaretDownIcon,
  TrashIcon,
} from "@phosphor-icons/react";
import { useEffect, useMemo, useState } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { Stat, StatStrip } from "../components/stat-strip";
import {
  formatCost,
  formatCount,
  formatTime,
  relativeTime,
  sessionTokens,
} from "../format";
import type {
  Message,
  SessionRow,
  SessionSnapshot,
  ToolCall,
  TranscriptPage,
} from "../types";

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

function setQuery(
  params: URLSearchParams,
  key: string,
  value: string,
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (value) next.set(key, value);
  else next.delete(key);
  if (key !== "after") {
    next.delete("after");
    next.delete("after_value");
  }
  return next;
}

export function SessionsPage() {
  const [params, setParams] = useSearchParams();
  const [search, setSearch] = useState(params.get("q") ?? "");
  const state = useAPI<SessionsResponse>(
    `/api/v1/app/sessions?${params.toString()}`,
  );
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
          setParams(setQuery(params, "q", search.trim()));
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
                  label: item.Name || item.Key,
                  count: item.Count,
                }))}
                active={params.get("project") ?? ""}
                onSelect={(value) =>
                  setParams(setQuery(params, "project", value))
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
                  setParams(setQuery(params, "agent", value))
                }
              />
              <FacetGroup
                title="Accounts"
                rows={(data.facets.Users ?? []).map((item) => ({
                  value: item.Value,
                  label: item.Value,
                  count: item.Count,
                }))}
                active={params.get("user") ?? ""}
                onSelect={(value) => setParams(setQuery(params, "user", value))}
              />
              <label className="check-row">
                <input
                  type="checkbox"
                  checked={params.get("subagents") === "1"}
                  onChange={(event) =>
                    setParams(
                      setQuery(
                        params,
                        "subagents",
                        event.target.checked ? "1" : "",
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
                        params,
                        "empty",
                        event.target.checked ? "1" : "",
                      ),
                    )
                  }
                />{" "}
                Include empty
              </label>
            </aside>
            <section className="feed">
              {(data.sessions ?? []).length === 0 ? (
                <div className="empty-state">
                  <h2>No matching sessions</h2>
                  <p>Clear a filter or search for a different phrase.</p>
                </div>
              ) : (
                (data.sessions ?? []).map((session) => (
                  <SessionFeedRow key={session.ID} session={session} />
                ))
              )}
              {data.has_more && (data.sessions ?? []).length > 0 ? (
                <button
                  type="button"
                  className="button secondary load-more"
                  onClick={() => {
                    const rows = data.sessions ?? [];
                    const last = rows[rows.length - 1];
                    if (last)
                      setParams(setQuery(params, "after", String(last.ID)));
                  }}
                >
                  Load next page
                </button>
              ) : null}
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

function SessionFeedRow({ session }: { session: SessionRow }) {
  const summary = session.Search.Text || session.Title || "Untitled session";
  return (
    <Link to={`/sessions/${session.ID}`} className="feed-row">
      <div className="feed-main">
        <span className="feed-project">
          {session.ProjectName || session.ProjectKey}
        </span>
        <strong>{summary}</strong>
        <span className="feed-meta">
          {session.Username} on {session.Machine || "unknown machine"}
          {session.GitBranch ? ` · ${session.GitBranch}` : ""}
        </span>
      </div>
      <div className="feed-signals">
        <span className="tag agent">{session.Agent}</span>
        {session.Grade ? (
          <span className={`tag grade grade-${session.Grade.toLowerCase()}`}>
            {session.Grade}
          </span>
        ) : null}
        {session.Outcome ? (
          <span className={`status ${session.Outcome}`}>{session.Outcome}</span>
        ) : null}
      </div>
      <div className="feed-numbers">
        <strong>
          {formatCost(session.TotalCostUSD, session.CostIncomplete)}
        </strong>
        <span>{formatCount(sessionTokens(session))} tokens</span>
        <span>{relativeTime(session.LastActiveAt)}</span>
      </div>
    </Link>
  );
}

type SessionResponse = {
  snapshot: SessionSnapshot;
  owner: boolean;
  can_delete: boolean;
};

export function SessionPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [nonce, setNonce] = useState(0);
  const state = useAPI<SessionResponse>(
    `/api/v1/app/sessions/${encodeURIComponent(id)}?revision=${nonce}`,
  );
  useEffect(() => {
    const events = new EventSource(
      `/sessions/${encodeURIComponent(id)}/events`,
    );
    events.addEventListener("update", () => setNonce((value) => value + 1));
    return () => events.close();
  }, [id]);
  return (
    <div className="page session-page">
      <AsyncView state={state}>
        {(data) => {
          const detail = data.snapshot.Audit.Detail;
          return (
            <>
              <header className="page-head session-head">
                <div>
                  <span className="crumb">
                    <Link to={`/projects/${detail.ProjectID}`}>
                      {detail.ProjectName || detail.ProjectKey}
                    </Link>{" "}
                    / session {detail.ID}
                  </span>
                  <h1>{detail.Title || `${detail.Agent} session`}</h1>
                  <p>
                    {detail.Username} · {detail.Machine || "unknown machine"} ·{" "}
                    {detail.GitBranch || "no branch"}
                  </p>
                </div>
                <div className="head-actions">
                  {data.owner ? (
                    <button
                      type="button"
                      className="button secondary"
                      onClick={async () => {
                        await request(
                          `/api/v1/app/sessions/${detail.ID}/publication`,
                          {
                            method: "PUT",
                            body: JSON.stringify({
                              published: detail.Visibility !== "public",
                            }),
                          },
                        );
                        setNonce((value) => value + 1);
                      }}
                    >
                      {detail.Visibility === "public" ? "Unpublish" : "Publish"}
                    </button>
                  ) : null}
                  {detail.PublicID ? (
                    <a
                      className="icon-link"
                      href={`/s/${detail.PublicID}`}
                      target="_blank"
                      rel="noreferrer"
                      aria-label="Open published session"
                    >
                      <ArrowSquareOutIcon />
                    </a>
                  ) : null}
                  {data.can_delete ? (
                    <button
                      type="button"
                      className="icon-link danger"
                      aria-label="Delete session"
                      onClick={async () => {
                        if (
                          !window.confirm(
                            "Delete this session and its parsed projection?",
                          )
                        )
                          return;
                        const result = await request<{ project_id: number }>(
                          `/api/v1/app/sessions/${detail.ID}`,
                          { method: "DELETE" },
                        );
                        navigate(`/projects/${result.project_id}`);
                      }}
                    >
                      <TrashIcon />
                    </button>
                  ) : null}
                </div>
              </header>
              <SessionInstruments snapshot={data.snapshot} />
              <Transcript
                initial={data.snapshot.Page}
                blobBase={`/api/v1/session/${detail.ID}/blob`}
                loadEarlier={async (before) =>
                  (
                    await request<{ page: TranscriptPage }>(
                      `/api/v1/app/sessions/${detail.ID}/transcript?before=${before}`,
                    )
                  ).page
                }
              />
            </>
          );
        }}
      </AsyncView>
    </div>
  );
}

function SessionInstruments({ snapshot }: { snapshot: SessionSnapshot }) {
  const detail = snapshot.Audit.Detail;
  const duration =
    detail.StartedAt && detail.EndedAt
      ? Math.max(
          0,
          new Date(detail.EndedAt).getTime() -
            new Date(detail.StartedAt).getTime(),
        )
      : 0;
  return (
    <StatStrip>
      <Stat label="Messages" value={formatCount(detail.MessageCount)} />
      <Stat label="Tokens" value={formatCount(sessionTokens(detail))} />
      <Stat
        label="Cost"
        value={formatCost(detail.TotalCostUSD, detail.CostIncomplete)}
      />
      <Stat
        label="Duration"
        value={duration ? formatDuration(duration) : "live"}
      />
      <Stat
        label="Tool calls"
        value={formatCount(snapshot.Tools?.length ?? 0)}
      />
      <Stat label="Started" value={relativeTime(detail.StartedAt)} />
    </StatStrip>
  );
}

function formatDuration(ms: number): string {
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return `${Math.floor(ms / 1_000)}s`;
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

function Transcript({
  initial,
  blobBase,
  loadEarlier,
}: {
  initial: TranscriptPage;
  blobBase: string;
  loadEarlier?: (before: number) => Promise<TranscriptPage>;
}) {
  const [page, setPage] = useState(initial);
  const [loading, setLoading] = useState(false);
  useEffect(() => setPage(initial), [initial]);
  const tools = useMemo(() => groupTools(page.Tools ?? []), [page.Tools]);
  return (
    <section className="transcript">
      <div className="section-head">
        <div>
          <h2>Transcript</h2>
          <p>
            {formatCount(page.Msgs?.length ?? 0)} messages in the current
            window.
          </p>
        </div>
      </div>
      {loadEarlier && page.HasEarlier && page.Msgs?.[0] ? (
        <button
          type="button"
          className="earlier"
          disabled={loading}
          onClick={async () => {
            setLoading(true);
            try {
              const earlier = await loadEarlier(page.Msgs?.[0]?.Ordinal ?? 0);
              setPage((current) => ({
                ...current,
                Msgs: [...(earlier.Msgs ?? []), ...(current.Msgs ?? [])],
                Tools: [...(earlier.Tools ?? []), ...(current.Tools ?? [])],
                Attachments: [
                  ...(earlier.Attachments ?? []),
                  ...(current.Attachments ?? []),
                ],
                HasEarlier: earlier.HasEarlier,
                EarlierCount: earlier.EarlierCount,
              }));
            } finally {
              setLoading(false);
            }
          }}
        >
          {loading
            ? "Loading..."
            : `Show ${formatCount(page.EarlierCount)} earlier messages`}
        </button>
      ) : null}
      <div className="message-list">
        {(page.Msgs ?? []).map((message) => (
          <MessageRow
            key={message.Ordinal}
            message={message}
            tools={tools.get(message.Ordinal) ?? []}
            blobBase={blobBase}
          />
        ))}
      </div>
    </section>
  );
}

function groupTools(tools: ToolCall[]): Map<number, ToolCall[]> {
  const grouped = new Map<number, ToolCall[]>();
  for (const tool of tools)
    grouped.set(tool.MessageOrdinal, [
      ...(grouped.get(tool.MessageOrdinal) ?? []),
      tool,
    ]);
  return grouped;
}

function MessageRow({
  message,
  tools,
  blobBase,
}: {
  message: Message;
  tools: ToolCall[];
  blobBase: string;
}) {
  return (
    <article
      className={`message role-${message.Role}`}
      id={`message-${message.Ordinal}`}
    >
      <header>
        <span className="message-role">{message.Role}</span>
        <span className="message-model">{message.Model}</span>
        <time>{formatTime(message.Timestamp)}</time>
        {message.Usage ? (
          <span className="message-usage">
            {formatCount(
              message.Usage.Input +
                message.Usage.Output +
                message.Usage.CacheRead +
                message.Usage.CacheWrite,
            )}{" "}
            tokens ·{" "}
            {formatCost(message.Usage.CostUSD, message.Usage.CostIncomplete)}
          </span>
        ) : null}
      </header>
      {message.DuplicatePrompt ? (
        <span className="tag warn">repeated prompt</span>
      ) : null}
      {message.ThinkingText ? (
        <details className="thinking">
          <summary>
            <CaretDownIcon size={13} /> Thinking
          </summary>
          <pre>{message.ThinkingText}</pre>
        </details>
      ) : null}
      {message.Content ? (
        <div className="message-content">{message.Content}</div>
      ) : null}
      {tools.length > 0 ? (
        <div className="tool-list">
          {tools.map((tool) => (
            <ToolCallRow
              key={`${tool.CallIndex}-${tool.ToolName}`}
              tool={tool}
              blobBase={blobBase}
            />
          ))}
        </div>
      ) : null}
    </article>
  );
}

function ToolCallRow({ tool, blobBase }: { tool: ToolCall; blobBase: string }) {
  const path = tool.FileRelPath || tool.FilePath || tool.Detail;
  return (
    <details
      className={`tool-call ${tool.ResultStatus === "error" ? "error" : ""}`}
    >
      <summary>
        <span className="tool-name">{tool.ToolName}</span>
        <span className="tool-detail">{path}</span>
        {tool.ResultStatus ? (
          <span className={`status ${tool.ResultStatus}`}>
            {tool.ResultStatus}
          </span>
        ) : null}
        <CaretDownIcon size={13} />
      </summary>
      <div className="tool-body-links">
        {tool.InputSHA ? (
          <a
            href={`${blobBase}/${tool.InputSHA}`}
            target="_blank"
            rel="noreferrer"
          >
            Open input ({formatCount(tool.InputBytes)} B)
          </a>
        ) : null}
        {tool.ResultSHA ? (
          <a
            href={`${blobBase}/${tool.ResultSHA}`}
            target="_blank"
            rel="noreferrer"
          >
            Open result ({formatCount(tool.ResultBytes)} B)
          </a>
        ) : null}
        {!tool.InputSHA && !tool.ResultSHA ? (
          <span className="muted">No captured body.</span>
        ) : null}
      </div>
    </details>
  );
}

export { Transcript };
