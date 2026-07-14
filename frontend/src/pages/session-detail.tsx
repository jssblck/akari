// The session detail page: header, publish/delete actions, the stat instrument
// band, subagents, the flow ribbon, the outline rail, and the transcript. Ported
// from session.templ. SSE drives a live session: on an "update" wake it fetches
// only the turns past the last rendered ordinal and splices them into the
// transcript (via Transcript's imperative handle) rather than refetching and
// discarding whatever the reader has scrolled into.
import { CopyIcon, TrashIcon } from "@phosphor-icons/react";
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { request, requestWithRetry, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { FlowRibbon } from "../components/flow-ribbon";
import { attempt } from "../components/notices";
import { OutlineRail } from "../components/outline-rail";
import {
  fallbackCategoryLabel,
  fallbackDeclinedObserved,
  fallbackModelsLabel,
  formatDurationSpan,
  gradeBand,
  hasHygieneSignal,
  isScored,
  outcomeLabel,
  qualityGrade,
  qualityScoreLabel,
  scoreBreakdownItems,
  thinkingBucketLabel,
  thinkingTokensLabel,
} from "../components/session-quality";
import { KindTag, SessionPublicTag } from "../components/session-tags";
import {
  asFallbacks,
  asSessionSignals,
  type ModelFallback,
  type SessionSignals,
} from "../components/session-types";
import { Stat } from "../components/stat-strip";
import { Transcript, type TranscriptHandle } from "../components/transcript";
import {
  formatCost,
  formatCount,
  formatPercent,
  formatTime,
  formatTokens,
  relativeTime,
} from "../format";
import "../sessions.css";
import { withBase } from "../base";
import type { SessionSnapshot, TranscriptPage } from "../types";

type SessionResponse = {
  snapshot: SessionSnapshot;
  owner: boolean;
  can_delete: boolean;
};

export function SessionPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const state = useAPI<SessionResponse>(
    `/api/v1/app/sessions/${encodeURIComponent(id)}`,
  );
  const [snapshot, setSnapshot] = useState<SessionSnapshot | null>(null);
  const [owner, setOwner] = useState(false);
  const [canDelete, setCanDelete] = useState(false);
  const transcriptRef = useRef<TranscriptHandle>(null);

  useEffect(() => {
    if (state.kind === "ready") {
      setSnapshot(state.data.snapshot);
      setOwner(state.data.owner);
      setCanDelete(state.data.can_delete);
    }
  }, [state]);

  // Live refresh: fetch only the turns past the last rendered ordinal and splice
  // them into the transcript, while replacing the audit/outline/flow shape whole
  // (those are small, bounded reads, and must reflect the latest projection). A
  // burst of "update" events collapses into one trailing refresh, matching the
  // old app.js's fetching/pending pair, so overlapping requests can never land
  // their swaps out of order.
  useEffect(() => {
    if (!id) return;
    const events = new EventSource(
      withBase(`/sessions/${encodeURIComponent(id)}/events`),
    );
    let fetching = false;
    let pending = false;
    async function refresh() {
      if (fetching) {
        pending = true;
        return;
      }
      fetching = true;
      try {
        const after = transcriptRef.current?.lastOrdinal();
        const query = after == null ? "0" : String(after);
        const result = await request<SessionResponse>(
          `/api/v1/app/sessions/${encodeURIComponent(id)}/append?after=${query}`,
        );
        setSnapshot((cur) =>
          cur
            ? {
                ...cur,
                Audit: result.snapshot.Audit,
                Outline: result.snapshot.Outline,
                Tools: result.snapshot.Tools,
                DupIDs: result.snapshot.DupIDs,
              }
            : result.snapshot,
        );
        setOwner(result.owner);
        setCanDelete(result.can_delete);
        if (after != null)
          transcriptRef.current?.appendPage(result.snapshot.Page);
      } finally {
        fetching = false;
        if (pending) {
          pending = false;
          refresh();
        }
      }
    }
    events.addEventListener("update", () => {
      refresh();
    });
    return () => events.close();
  }, [id]);

  return (
    <div className="page session-page">
      <AsyncView state={state}>
        {() => {
          if (!snapshot) return null;
          const detail = snapshot.Audit.Detail;
          const signals = asSessionSignals(snapshot.Audit.Signals);
          const fallbacks = asFallbacks(snapshot.Audit.Fallbacks);
          return (
            <>
              <header className="page-head session-head">
                <div className="session-title">
                  <h1>
                    <Link to={`/projects/${detail.ProjectID}`}>
                      {detail.ProjectName || detail.ProjectKey}
                    </Link>
                    <span className="sep">/</span>
                    <span className="muted">session #{detail.ID}</span>
                  </h1>
                  {detail.Title ? (
                    <div
                      className="session-subtitle muted"
                      title={detail.Title}
                    >
                      {detail.Title}
                    </div>
                  ) : null}
                  <div className="session-meta">
                    <span className="tag agent">{detail.Agent}</span>
                    <KindTag kind={detail.ProjectKind} />
                    <SessionPublicTag
                      visibility={detail.Visibility}
                      publicID={detail.PublicID}
                    />
                    {snapshot.DupIDs > 0 ? (
                      <span
                        className="tag warn"
                        title="Repeated tool-call ids: a resumed or compacted transcript replaying earlier turns."
                      >
                        {snapshot.DupIDs === 1
                          ? "1 duplicate id"
                          : `${snapshot.DupIDs} duplicate ids`}
                      </span>
                    ) : null}
                    {detail.GitBranch ? (
                      <span className="muted">{detail.GitBranch}</span>
                    ) : null}
                    <span className="muted">{detail.Username}</span>
                    <span className="muted">{detail.Machine}</span>
                  </div>
                </div>
                <SessionActions
                  detail={detail}
                  owner={owner}
                  canDelete={canDelete}
                  onPublicationChanged={(published, publicID) =>
                    setSnapshot((cur) =>
                      cur
                        ? {
                            ...cur,
                            Audit: {
                              ...cur.Audit,
                              Detail: {
                                ...cur.Audit.Detail,
                                Visibility: published ? "public" : "private",
                                PublicID: publicID,
                              },
                            },
                          }
                        : cur,
                    )
                  }
                  onDeleted={(projectID) => navigate(`/projects/${projectID}`)}
                />
              </header>
              <SessionStats
                detail={detail}
                signals={signals}
                fallbacks={fallbacks}
              />
              <SubagentsSection subagents={snapshot.Audit.Subagents ?? []} />
              <div className="session-grid">
                <OutlineRail
                  outline={snapshot.Outline ?? []}
                  toolsByOrdinal={groupByOrdinal(snapshot.Tools ?? [])}
                  blobBase={withBase(`/api/v1/session/${detail.ID}/blob`)}
                />
                <div className="session-maincol">
                  <FlowRibbon
                    outline={snapshot.Outline ?? []}
                    toolsByOrdinal={groupByOrdinal(snapshot.Tools ?? [])}
                  />
                  <Transcript
                    ref={transcriptRef}
                    initial={snapshot.Page}
                    blobBase={withBase(`/api/v1/session/${detail.ID}/blob`)}
                    agent={detail.Agent}
                    loadEarlier={async (before) =>
                      (
                        await requestWithRetry<{ page: TranscriptPage }>(
                          `/api/v1/app/sessions/${detail.ID}/transcript?before=${before}`,
                        )
                      ).page
                    }
                  />
                </div>
              </div>
            </>
          );
        }}
      </AsyncView>
    </div>
  );
}

function groupByOrdinal<T extends { MessageOrdinal: number }>(
  rows: T[],
): Map<number, T[]> {
  const grouped = new Map<number, T[]>();
  for (const row of rows)
    grouped.set(row.MessageOrdinal, [
      ...(grouped.get(row.MessageOrdinal) ?? []),
      row,
    ]);
  return grouped;
}

function SessionActions({
  detail,
  owner,
  canDelete,
  onPublicationChanged,
  onDeleted,
}: {
  detail: SessionSnapshot["Audit"]["Detail"];
  owner: boolean;
  canDelete: boolean;
  onPublicationChanged: (published: boolean, publicID: string | null) => void;
  onDeleted: (projectID: number) => void;
}) {
  const [copied, setCopied] = useState(false);
  const shareURL = detail.PublicID
    ? `${window.location.origin}/s/${detail.PublicID}`
    : "";
  return (
    <div className="session-actions">
      {owner ? (
        <>
          {detail.Visibility === "public" && detail.PublicID ? (
            <>
              <a
                className="share-link muted small"
                href={withBase(`/s/${detail.PublicID}`)}
                target="_blank"
                rel="noreferrer"
                title="Public share link"
              >
                {`/s/${detail.PublicID}`}
              </a>
              <button
                type="button"
                className="icon-link"
                aria-label={copied ? "Copied" : "Copy link"}
                title={copied ? "Copied" : "Copy link"}
                onClick={async () => {
                  await navigator.clipboard.writeText(shareURL);
                  setCopied(true);
                  window.setTimeout(() => setCopied(false), 1400);
                }}
              >
                <CopyIcon />
              </button>
            </>
          ) : null}
          <button
            type="button"
            className="button secondary"
            title={
              detail.Visibility === "public"
                ? "Removes the public link. Publishing again mints a new link, so the old one stays dead."
                : "Mint a public share link."
            }
            onClick={async () => {
              const publish = detail.Visibility !== "public";
              const ok = await attempt(
                request<{ published: boolean; public_id?: string }>(
                  `/api/v1/app/sessions/${detail.ID}/publication`,
                  {
                    method: "PUT",
                    body: JSON.stringify({ published: publish }),
                  },
                ).then((result) =>
                  onPublicationChanged(
                    result.published,
                    result.public_id ?? null,
                  ),
                ),
                publish ? "Session published." : "Session unpublished.",
              );
              if (!ok) return;
            }}
          >
            {detail.Visibility === "public" ? "Unpublish" : "Publish"}
          </button>
        </>
      ) : null}
      {canDelete ? (
        <button
          type="button"
          className="icon-link danger"
          aria-label="Delete session"
          title="Deletes this session permanently."
          onClick={async () => {
            if (
              !window.confirm("Delete this session and its parsed projection?")
            )
              return;
            await attempt(
              request<{ project_id: number }>(
                `/api/v1/app/sessions/${detail.ID}`,
                { method: "DELETE" },
              ).then((result) => onDeleted(result.project_id)),
            );
          }}
        >
          <TrashIcon />
        </button>
      ) : null}
    </div>
  );
}

function SessionStats({
  detail,
  signals,
  fallbacks,
}: {
  detail: SessionSnapshot["Audit"]["Detail"];
  signals: SessionSignals;
  fallbacks: ModelFallback[];
}) {
  const totalTokens =
    detail.TotalInput +
    detail.TotalOutput +
    detail.TotalCacheRead +
    detail.TotalCacheWrite;
  const promptTokens =
    detail.TotalInput + detail.TotalCacheRead + detail.TotalCacheWrite;
  const hitRate = promptTokens > 0 ? detail.TotalCacheRead / promptTokens : 0;
  return (
    <div className="stats session-stats" id="session-stats">
      <Stat label="Messages" value={formatCount(detail.MessageCount)} />
      <Stat label="User msgs" value={formatCount(detail.UserMessageCount)} />
      <StatTip label="Tokens" value={formatTokens(totalTokens)}>
        <dl className="tt-grid">
          <dt>Input</dt>
          <dd>{formatTokens(detail.TotalInput)}</dd>
          <dt>Output</dt>
          <dd>{formatTokens(detail.TotalOutput)}</dd>
          <dt>Cache read</dt>
          <dd>{formatTokens(detail.TotalCacheRead)}</dd>
          <dt>Cache write</dt>
          <dd>{formatTokens(detail.TotalCacheWrite)}</dd>
        </dl>
        {signals.PeakContextTokens !== null ? (
          <>
            <div className="tt-sub">Context</div>
            <dl className="tt-grid">
              <dt>Peak context</dt>
              <dd>{formatTokens(signals.PeakContextTokens)}</dd>
              <dt>Resets</dt>
              <dd>{signals.ContextResetCount ?? 0}</dd>
            </dl>
            <div className="tt-note muted small">
              Peak is the heaviest single turn's prompt: input, cache read, and
              cache write, output excluded. It measures context load, not spend.
            </div>
          </>
        ) : null}
        <div className="tt-cost">
          {formatCost(detail.TotalCostUSD, detail.CostIncomplete)}
        </div>
      </StatTip>
      <StatTip label="Cache" value={formatPercent(hitRate)}>
        <dl className="tt-grid">
          <dt>Hit rate</dt>
          <dd>{formatPercent(hitRate)}</dd>
          <dt>Cache read</dt>
          <dd>{formatTokens(detail.TotalCacheRead)}</dd>
          <dt>Cache write</dt>
          <dd>{formatTokens(detail.TotalCacheWrite)}</dd>
          <dt>Uncached in</dt>
          <dd>{formatTokens(detail.TotalInput)}</dd>
        </dl>
        <div className="tt-cost">
          {(detail.TotalCacheSavingsUSD < 0 ? "cost " : "saved ") +
            formatCost(Math.abs(detail.TotalCacheSavingsUSD), false) +
            (detail.CacheSavingsIncomplete ? " partial" : "")}
        </div>
      </StatTip>
      <QualityStat signals={signals} />
      {detail.ModelFallbackCount > 0 ? (
        <FallbackStat count={detail.ModelFallbackCount} fallbacks={fallbacks} />
      ) : null}
      <Stat
        label="Duration"
        value={formatDurationSpan(detail.StartedAt, detail.EndedAt)}
      />
      <Stat label="Started" value={relativeTime(detail.StartedAt)} />
    </div>
  );
}

// StatTip is a stat tile whose value carries a hover/focus breakdown card, the
// session page's own flavor of the shared hover-tip pattern: the tile itself
// (not a wrapping span) is the trigger, so it lays out as a stat-strip cell.
function StatTip({
  label,
  value,
  children,
}: {
  label: string;
  value: string;
  children: React.ReactNode;
}) {
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: the stat tile is the tooltip trigger, so it must be focusable to reach the breakdown by keyboard (matches HoverTip's convention).
    <div className="stat" tabIndex={0}>
      <span className="label">{label}</span>
      <strong>{value}</strong>
      <div className="tip-card" role="tooltip">
        {children}
      </div>
    </div>
  );
}

function QualityStat({ signals }: { signals: SessionSignals }) {
  const breakdown = scoreBreakdownItems(signals);
  return (
    <div
      className={`stat quality-stat q-${gradeBand(signals.Grade)}`}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: the stat tile is the tooltip trigger, so it must be focusable to reach the breakdown by keyboard (matches HoverTip's convention).
      tabIndex={0}
    >
      <span className="label">Quality</span>
      <strong>{qualityGrade(signals)}</strong>
      <div className="tip-card" role="tooltip">
        <dl className="tt-grid">
          <dt>Outcome</dt>
          <dd>{outcomeLabel(signals.Outcome)}</dd>
          <dt>Confidence</dt>
          <dd>{signals.OutcomeConfidence}</dd>
          <dt>Score</dt>
          <dd>{qualityScoreLabel(signals)}</dd>
          {signals.ToolCalls > 0 ? (
            <>
              <dt>Failures</dt>
              <dd>
                {signals.ToolFailures} / {signals.ToolCalls}
              </dd>
              <dt>Retries</dt>
              <dd>{signals.ToolRetries}</dd>
              <dt>Edit churn</dt>
              <dd>{signals.EditChurn}</dd>
              <dt>Longest fail streak</dt>
              <dd>{signals.LongestFailureStreak}</dd>
            </>
          ) : null}
        </dl>
        {!isScored(signals) ? (
          <div className="tt-note muted small">
            Not graded yet. A session is graded once it settles.
          </div>
        ) : null}
        {hasHygieneSignal(signals) ? (
          <>
            <div className="tt-sub">Input</div>
            <dl className="tt-grid">
              {signals.ShortPromptCount > 0 ? (
                <>
                  <dt>Terse prompts</dt>
                  <dd>{signals.ShortPromptCount}</dd>
                </>
              ) : null}
              {signals.DuplicatePromptCount > 0 ? (
                <>
                  <dt>Repeated</dt>
                  <dd>{signals.DuplicatePromptCount}</dd>
                </>
              ) : null}
              {signals.NoCodeContextCount > 0 ? (
                <>
                  <dt>No code pointer</dt>
                  <dd>{signals.NoCodeContextCount}</dd>
                </>
              ) : null}
              {signals.UnstructuredStart ? (
                <>
                  <dt>Opening</dt>
                  <dd>terse</dd>
                </>
              ) : null}
            </dl>
          </>
        ) : null}
        {signals.AssistantTurns !== null ? (
          <>
            <div className="tt-sub">Thinking</div>
            <dl className="tt-grid">
              <dt>Observed</dt>
              <dd>
                {thinkingBucketLabel(
                  (signals.ThinkingTurns ?? 0) > 0 &&
                    signals.ThinkingTailTokens !== null
                    ? bucketFor(signals.ThinkingTailTokens)
                    : "off",
                )}
              </dd>
              {(signals.ThinkingTurns ?? 0) > 0 ? (
                <>
                  <dt>Hard-turn mean</dt>
                  <dd>
                    {thinkingTokensLabel(signals.ThinkingTailTokens ?? 0)}
                  </dd>
                  <dt>Hardest turn</dt>
                  <dd>
                    {thinkingTokensLabel(signals.ThinkingPeakTokens ?? 0)}
                  </dd>
                  <dt>Reasoned</dt>
                  <dd>
                    {signals.ThinkingTurns} of {signals.AssistantTurns} turns
                  </dd>
                </>
              ) : null}
            </dl>
            <div className="tt-note muted small">
              Observed deliberation on an absolute token scale: how hard the
              model actually thought, not a configured level. Per-turn tokens
              are exact where the agent reports them, else estimated from the
              reasoning trace.
            </div>
          </>
        ) : null}
        {isScored(signals) ? (
          <>
            <div className="tt-sub">Score arithmetic</div>
            <dl className="tt-grid tt-score">
              {breakdown.length === 0 ? (
                <>
                  <dt>No penalties</dt>
                  <dd className="mono">-0</dd>
                </>
              ) : (
                breakdown.map((item) => (
                  <>
                    <dt key={`${item.label}-label`}>{item.label}</dt>
                    <dd key={`${item.label}-points`} className="mono">
                      -{item.points}
                    </dd>
                  </>
                ))
              )}
              <dt>Score</dt>
              <dd className="mono">{signals.Score}</dd>
            </dl>
          </>
        ) : null}
      </div>
    </div>
  );
}

function bucketFor(
  tailTokens: number,
): "off" | "low" | "medium" | "high" | "xhigh" {
  if (tailTokens <= 128) return "low";
  if (tailTokens <= 512) return "medium";
  if (tailTokens <= 2048) return "high";
  return "xhigh";
}

function FallbackStat({
  count,
  fallbacks,
}: {
  count: number;
  fallbacks: ModelFallback[];
}) {
  const shown = fallbacks.slice(0, 5);
  const overflow = count > shown.length ? count - shown.length : 0;
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: the stat tile is the tooltip trigger, so it must be focusable to reach the breakdown by keyboard (matches HoverTip's convention).
    <div className="stat fallback-stat" tabIndex={0}>
      <span className="label">Fallbacks</span>
      <strong>{count}</strong>
      <div className="tip-card" role="tooltip">
        {shown.map((f, i) => (
          <div key={f.DedupKey || i}>
            {i > 0 ? <div className="tt-sub">Fallback</div> : null}
            <dl className="tt-grid">
              <dt>Models</dt>
              <dd>{fallbackModelsLabel(f)}</dd>
              <dt>Category</dt>
              <dd>{fallbackCategoryLabel(f)}</dd>
              {fallbackDeclinedObserved(f) ? (
                <>
                  <dt>Declined input</dt>
                  <dd>{formatTokens(f.DeclinedInput ?? 0)}</dd>
                  <dt>Declined output</dt>
                  <dd>{formatTokens(f.DeclinedOutput ?? 0)}</dd>
                  <dt>Declined cache write</dt>
                  <dd>{formatTokens(f.DeclinedCacheWrite ?? 0)}</dd>
                  <dt>Declined cache read</dt>
                  <dd>{formatTokens(f.DeclinedCacheRead ?? 0)}</dd>
                </>
              ) : null}
              <dt>Time</dt>
              <dd>{f.OccurredAt ? formatTime(f.OccurredAt) : "-"}</dd>
            </dl>
          </div>
        ))}
        {overflow > 0 ? (
          <div className="tt-note">plus {overflow} more</div>
        ) : null}
      </div>
    </div>
  );
}

const SUBAGENT_COLLAPSE_THRESHOLD = 8;

function SubagentsSection({
  subagents,
}: {
  subagents: SessionSnapshot["Audit"]["Subagents"];
}) {
  const rows = subagents ?? [];
  if (rows.length === 0) return null;
  const collapsed = rows.length > SUBAGENT_COLLAPSE_THRESHOLD;
  const table = <SubagentsTable rows={rows} />;
  return (
    <div id="session-subagents">
      <h2>Subagents</h2>
      {collapsed ? (
        <details className="subagents subagents-fold">
          <summary className="subagents-summary">
            <span className="subagents-summary-label">
              {subagentsSummaryLabel(rows)}
            </span>
            <span
              className="subagents-summary-hint muted small"
              aria-hidden="true"
            />
          </summary>
          {table}
        </details>
      ) : (
        <div className="subagents">{table}</div>
      )}
    </div>
  );
}

function subagentsSummaryLabel(
  rows: NonNullable<SessionSnapshot["Audit"]["Subagents"]>,
): string {
  let cost = 0;
  let incomplete = false;
  let failed = 0;
  for (const r of rows) {
    cost += r.TotalCostUSD;
    incomplete = incomplete || r.CostIncomplete;
    if (r.Outcome === "errored") failed++;
  }
  const unit = rows.length === 1 ? "subagent" : "subagents";
  let label = `${rows.length} ${unit} · ${formatCost(cost, incomplete)}`;
  if (failed > 0) label += ` · ${failed} failed`;
  return label;
}

function SubagentsTable({
  rows,
}: {
  rows: NonNullable<SessionSnapshot["Audit"]["Subagents"]>;
}) {
  const navigate = useNavigate();
  return (
    <div className="table-wrap">
      <table className="data-table">
        <thead>
          <tr>
            <th>Task</th>
            <th>Agent</th>
            <th>Verdict</th>
            <th className="num">Messages</th>
            <th className="num">Cost</th>
            <th>Updated</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((s) => (
            <tr
              key={s.ID}
              className="row-link"
              onClick={(ev) => {
                // A nested link or the public-tag anchor owns its own click (the
                // whole row is the hit target, but a real control inside it wins);
                // navigating the row too would race a new-tab open against this
                // tab's own navigation.
                if ((ev.target as HTMLElement).closest("a, button")) return;
                navigate(`/sessions/${s.ID}`);
              }}
            >
              <td className="sub-task">
                <Link to={`/sessions/${s.ID}`} title={s.Title}>
                  {s.Title || `${s.Agent} session`}
                </Link>
                <SessionPublicTag
                  visibility={s.Visibility}
                  publicID={s.PublicID}
                />
              </td>
              <td>
                <span className="tag agent">{s.Agent}</span>
              </td>
              <td className="sub-verdict">
                {s.Outcome === "abandoned" || s.Outcome === "errored" ? (
                  <span
                    className={`sub-outcome tone-${s.Outcome === "errored" ? "err" : "warn"}`}
                  >
                    {s.Outcome}
                  </span>
                ) : s.Outcome === "completed" ? (
                  <span className="sub-outcome muted">completed</span>
                ) : null}
                {s.Grade ? (
                  <span
                    className={`tag grade grade-${s.Grade.toLowerCase()}`}
                    title="quality grade"
                  >
                    {s.Grade}
                  </span>
                ) : null}
              </td>
              <td className="num">{s.MessageCount}</td>
              <td className="num">
                {formatCost(s.TotalCostUSD, s.CostIncomplete)}
              </td>
              <td className="muted">{formatTime(s.LastActiveAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
