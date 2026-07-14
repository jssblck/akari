// The transcript: the session's windowed message list, with per-turn instruments
// (reply latency, context-shed dividers, duplicate-prompt and hygiene tags,
// thinking bands), tool chips wired to the inspector modal, and inline
// attachments. Shared by the authenticated session page and the public session
// page (frontend/src/pages/public.tsx imports Transcript directly), so every
// change here reaches both. Ported from session.templ's transcriptMsg family and
// the TranscriptWalker in internal/server/web/session_metrics.go.
import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useMemo,
  useState,
} from "react";

import { formatCost, formatCount, formatTime, formatTokens } from "../format";
import type { Attachment, Message, ToolCall, TranscriptPage } from "../types";
import {
  contextLabel,
  contextStamp,
  detailLabel,
  fallbackNoticeLabel,
  isContextReset,
  messageThinkingBand,
  shedLabel,
  thinkingBucketLabel,
  toolFilePath,
  toolFileTitle,
  turnCostLabel,
  turnLatency,
  turnTokenTotal,
} from "./session-quality";
import {
  asFallbacks,
  asFullMessage,
  type ModelFallback,
  type TurnUsageFull,
} from "./session-types";
import { openToolInspector, ToolInspectorModal } from "./tool-inspector";

type ShedMark = {
  fromTokens: number;
  toTokens: number;
  fromUsage: TurnUsageFull;
  toUsage: TurnUsageFull;
};
type MsgMetrics = { latency: number; shed: ShedMark | null };

// walkMetrics ports TranscriptWalker: it primes on the seed rows (context that
// precedes the window, never rendered) then walks the rendered rows in order,
// carrying only the pending-prompt anchor and the previous measured turn's usage,
// so it costs O(window) rather than a second session-sized structure.
function walkMetrics(
  seed: Message[],
  msgs: Message[],
): Map<number, MsgMetrics> {
  let anchor: Date | null = null;
  let prevContext = 0;
  let prevUsage: TurnUsageFull | null = null;
  let havePrev = false;
  const out = new Map<number, MsgMetrics>();

  function next(m: Message, record: boolean) {
    let latency = 0;
    if (m.Role === "user") {
      anchor = m.Timestamp ? new Date(m.Timestamp) : null;
    } else if (m.Role === "assistant" && anchor && m.Timestamp) {
      const d = new Date(m.Timestamp).getTime() - anchor.getTime();
      anchor = null;
      if (d >= 0) latency = d;
    }
    let shed: ShedMark | null = null;
    const usage = asFullMessage(m).Usage;
    if (usage) {
      if (
        havePrev &&
        prevUsage &&
        isContextReset(prevContext, usage.ContextTokens)
      ) {
        shed = {
          fromTokens: prevContext,
          toTokens: usage.ContextTokens,
          fromUsage: prevUsage,
          toUsage: usage,
        };
      }
      prevContext = usage.ContextTokens;
      prevUsage = usage;
      havePrev = true;
    }
    if (record) out.set(m.Ordinal, { latency, shed });
  }

  for (const m of seed) next(m, false);
  for (const m of msgs) next(m, true);
  return out;
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

function fallbacksByOrdinal(fbs: ModelFallback[]): Map<number, ModelFallback> {
  const out = new Map<number, ModelFallback>();
  for (const f of fbs) {
    if (f.MessageOrdinal === null) continue;
    if (!out.has(f.MessageOrdinal)) out.set(f.MessageOrdinal, f);
  }
  return out;
}

function isImageMedia(mediaType: string): boolean {
  return mediaType.startsWith("image/");
}

function chipLabel(mediaType: string, bytes: number): string {
  const short =
    mediaType === "application/json"
      ? "json"
      : mediaType === "text/plain"
        ? "text"
        : mediaType || "data";
  return `${formatBytes(bytes)} ${short}`;
}

function formatBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// TranscriptHandle is the imperative surface the session detail page uses to
// splice a live SSE append into an already-loaded transcript. A normal prop
// (a new `initial`) is the wrong tool for this: Transcript resets its whole page
// state whenever `initial` changes identity, which is right for a first load but
// would blow away every "Show earlier" page the reader had already loaded. The
// handle lets the parent push just the new tail in, leaving loaded history alone.
export type TranscriptHandle = {
  lastOrdinal: () => number | null;
  appendPage: (fragment: TranscriptPage) => void;
};

export const Transcript = forwardRef<
  TranscriptHandle,
  {
    initial: TranscriptPage;
    blobBase: string;
    loadEarlier?: (before: number) => Promise<TranscriptPage>;
    // agent picks the reasoning-trace bytes-per-token factor for the per-turn
    // thinking-band chip (see quality.ThinkingBytesPerToken). Optional so the public
    // session page, which does not carry the session's agent through Transcript's
    // props, still renders a reasonable estimate on the default (Claude) factor.
    agent?: string;
  }
>(function Transcript(
  { initial, blobBase, loadEarlier, agent = "claude" },
  ref,
) {
  const [page, setPage] = useState(initial);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  useEffect(() => setPage(initial), [initial]);

  useImperativeHandle(
    ref,
    () => ({
      lastOrdinal: () => {
        const msgs = page.Msgs ?? [];
        return msgs.length > 0
          ? (msgs[msgs.length - 1]?.Ordinal ?? null)
          : null;
      },
      appendPage: (fragment) => {
        if ((fragment.Msgs ?? []).length === 0) return;
        setPage((cur) => ({
          ...cur,
          Msgs: [...(cur.Msgs ?? []), ...(fragment.Msgs ?? [])],
          Tools: [...(cur.Tools ?? []), ...(fragment.Tools ?? [])],
          Attachments: [
            ...(cur.Attachments ?? []),
            ...(fragment.Attachments ?? []),
          ],
          Fallbacks: [...(cur.Fallbacks ?? []), ...(fragment.Fallbacks ?? [])],
        }));
      },
    }),
    [page.Msgs],
  );

  const toolsByOrdinal = useMemo(
    () => groupByOrdinal(page.Tools ?? []),
    [page.Tools],
  );
  const attachmentsByOrdinal = useMemo(
    () => groupByOrdinal(page.Attachments ?? []),
    [page.Attachments],
  );
  const fallbacks = useMemo(
    () => fallbacksByOrdinal(asFallbacks(page.Fallbacks)),
    [page.Fallbacks],
  );
  const metrics = useMemo(
    () => walkMetrics(page.Seed ?? [], page.Msgs ?? []),
    [page.Seed, page.Msgs],
  );

  return (
    <section className="transcript">
      <ToolInspectorModal />
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
          className="transcript-earlier"
          disabled={loading}
          onClick={async () => {
            setLoading(true);
            setLoadError("");
            try {
              const earlier = await loadEarlier(page.Msgs?.[0]?.Ordinal ?? 0);
              setPage((cur) => ({
                ...cur,
                Msgs: [...(earlier.Msgs ?? []), ...(cur.Msgs ?? [])],
                Seed: earlier.Seed ?? [],
                Tools: [...(earlier.Tools ?? []), ...(cur.Tools ?? [])],
                Attachments: [
                  ...(earlier.Attachments ?? []),
                  ...(cur.Attachments ?? []),
                ],
                Fallbacks: [
                  ...(earlier.Fallbacks ?? []),
                  ...(cur.Fallbacks ?? []),
                ],
                HasEarlier: earlier.HasEarlier,
                EarlierCount: earlier.EarlierCount,
              }));
            } catch (error) {
              setLoadError(
                error instanceof Error
                  ? error.message
                  : "Could not load earlier messages.",
              );
            } finally {
              setLoading(false);
            }
          }}
        >
          <span>{loading ? "Loading…" : "Show earlier"}</span>
          {!loading ? (
            <span className="muted small">
              {formatCount(page.EarlierCount)} earlier
            </span>
          ) : null}
        </button>
      ) : null}
      {loadError ? (
        <p className="form-error" role="alert">
          {loadError}
        </p>
      ) : null}
      {(page.Msgs ?? []).length === 0 ? (
        <div className="empty">No messages parsed yet.</div>
      ) : (
        (page.Msgs ?? []).map((message) => (
          <MessageRow
            key={message.Ordinal}
            message={message}
            metrics={metrics.get(message.Ordinal) ?? { latency: 0, shed: null }}
            tools={toolsByOrdinal.get(message.Ordinal) ?? []}
            attachments={attachmentsByOrdinal.get(message.Ordinal) ?? []}
            fallback={fallbacks.get(message.Ordinal)}
            agent={agent}
            blobBase={blobBase}
          />
        ))
      )}
    </section>
  );
});

function MessageRow({
  message,
  metrics,
  tools,
  attachments,
  fallback,
  agent,
  blobBase,
}: {
  message: Message;
  metrics: MsgMetrics;
  tools: ToolCall[];
  attachments: Attachment[];
  fallback: ModelFallback | undefined;
  agent: string;
  blobBase: string;
}) {
  const full = asFullMessage(message);
  return (
    <>
      {metrics.shed ? <ShedDivider shed={metrics.shed} /> : null}
      {fallback ? <FallbackNotice fallback={fallback} /> : null}
      {message.Role === "context" ? (
        <ContextTurn message={message} />
      ) : (
        <MessageTurn
          message={full}
          metrics={metrics}
          tools={tools}
          attachments={attachments}
          agent={agent}
          blobBase={blobBase}
        />
      )}
    </>
  );
}

function ContextTurn({ message }: { message: Message }) {
  return (
    <details
      className="msg msg-context context-turn"
      id={`msg-${message.Ordinal}`}
      data-ordinal={message.Ordinal}
    >
      <summary className="context-summary">
        <span className="role">context</span>
        <span className="tag context-kind">
          {contextLabel(message.Content)}
        </span>
        <span className="spacer" />
        <span className="muted small">{formatTime(message.Timestamp)}</span>
      </summary>
      {message.Content ? (
        <div className="content">{message.Content}</div>
      ) : null}
    </details>
  );
}

function MessageTurn({
  message,
  metrics,
  tools,
  attachments,
  agent,
  blobBase,
}: {
  message: ReturnType<typeof asFullMessage>;
  metrics: MsgMetrics;
  tools: ToolCall[];
  attachments: Attachment[];
  agent: string;
  blobBase: string;
}) {
  const band =
    message.Role === "assistant"
      ? messageThinkingBand(
          agent,
          message.HasThinking,
          message.ThinkingBytes,
          message.Usage?.Reasoning ?? 0,
        )
      : null;
  return (
    <div
      className={`msg role-${message.Role}`}
      id={`msg-${message.Ordinal}`}
      data-ordinal={message.Ordinal}
    >
      <div className="meta">
        <span className="role">{message.Role}</span>
        {message.Model ? (
          <span className="tag model">{message.Model}</span>
        ) : null}
        {message.Role === "user" && message.PromptFactsCurrent ? (
          <>
            {message.PromptShort ? (
              <span
                className="tag hygiene"
                title="under 4 words: give the agent something to grip"
              >
                terse
              </span>
            ) : null}
            {message.PromptNoCode ? (
              <span
                className="tag hygiene"
                title="a change request with no file, path, or code anchor"
              >
                no code pointer
              </span>
            ) : null}
            {message.DuplicatePrompt ? (
              <span
                className="tag hygiene"
                title="verbatim repeat of an earlier prompt"
              >
                repeat
              </span>
            ) : null}
          </>
        ) : null}
        <span className="spacer" />
        <span className="meta-metrics">
          {metrics.latency > 0 ? (
            <span
              className="stamp-latency mono"
              title="time from the prompt to this reply"
            >
              {turnLatency(metrics.latency)}
            </span>
          ) : null}
          {message.Usage ? (
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the tok-cell tooltip trigger must be focusable so the breakdown is reachable by keyboard (matches HoverTip's convention).
            <span className="tok-cell turn-metrics" tabIndex={0}>
              <span className="stamp-ctx mono">
                {contextStamp(message.Usage)}
              </span>
              {message.Usage.CostUSD !== null &&
              message.Usage.CostUSD !== undefined ? (
                <span className="stamp-cost mono">
                  {formatCost(
                    message.Usage.CostUSD,
                    message.Usage.CostIncomplete,
                  )}
                </span>
              ) : null}
              <TurnCard usage={message.Usage} />
            </span>
          ) : null}
        </span>
        <time className="muted small">{formatTime(message.Timestamp)}</time>
      </div>
      {band ? (
        <div
          className={`thinking-band band-${band}`}
          title="observed deliberation on this turn, on an absolute token scale (exact where the agent reports it, else estimated from the reasoning trace)"
        >
          <span className="thinking-band-dot" />
          <span className="thinking-band-label">
            thinking: {thinkingBucketLabel(band)}
          </span>
        </div>
      ) : null}
      {message.HasThinking && message.ThinkingText ? (
        <details className="thinking">
          <summary>Thinking</summary>
          <div className="thinking-body">{message.ThinkingText}</div>
        </details>
      ) : null}
      {message.Content ? (
        <div className="content">{message.Content}</div>
      ) : null}
      {attachments.length > 0 ? (
        <div className="attachments">
          {attachments.map((a) => (
            <AttachmentTile key={a.SHA256} attachment={a} blobBase={blobBase} />
          ))}
        </div>
      ) : null}
      {tools.length > 0 ? (
        <div className="tools">
          {tools.map((tool) => (
            <ToolChip
              key={`${tool.CallIndex}-${tool.ToolName}`}
              tool={tool}
              blobBase={blobBase}
            />
          ))}
        </div>
      ) : null}
    </div>
  );
}

function TurnCard({ usage }: { usage: TurnUsageFull }) {
  return (
    <span className="tok-tip" role="tooltip">
      <span className="tt-total">
        {formatTokens(turnTokenTotal(usage))} tokens
      </span>
      <dl className="tt-grid">
        <dt>In</dt>
        <dd>{formatTokens(usage.Input)}</dd>
        <dt>Out</dt>
        <dd>{formatTokens(usage.Output)}</dd>
        <dt>Cache read</dt>
        <dd>{formatTokens(usage.CacheRead)}</dd>
        <dt>Cache write</dt>
        <dd>{formatTokens(usage.CacheWrite)}</dd>
        {usage.Reasoning > 0 ? (
          <>
            <dt>Reasoning</dt>
            <dd>{formatTokens(usage.Reasoning)}</dd>
          </>
        ) : null}
        <dt>Context</dt>
        <dd>{formatTokens(usage.ContextTokens)}</dd>
      </dl>
      <span className="tt-cost">{turnCostLabel(usage, formatCost)}</span>
    </span>
  );
}

function ShedDivider({ shed }: { shed: ShedMark }) {
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: the divider's visible label is also its tooltip trigger, so it must be focusable to reach the breakdown by keyboard (matches HoverTip's convention). The visible shed-label text supplies the accessible name.
    <div className="msg-shed tok-cell" tabIndex={0}>
      <span className="shed-label mono">
        {shedLabel(shed.fromTokens, shed.toTokens)}
      </span>
      <span className="tok-tip shed-tip" role="tooltip">
        <span className="tt-total">context shed</span>
        <UsageGrid usage={shed.fromUsage} label="Before" />
        <span className="tt-cost">
          {turnCostLabel(shed.fromUsage, formatCost)}
        </span>
        <UsageGrid usage={shed.toUsage} label="After" className="shed-after" />
        <span className="tt-cost">
          {turnCostLabel(shed.toUsage, formatCost)}
        </span>
      </span>
    </div>
  );
}

function UsageGrid({
  usage,
  label,
  className,
}: {
  usage: TurnUsageFull;
  label: string;
  className?: string;
}) {
  return (
    <dl className={`tt-grid${className ? ` ${className}` : ""}`}>
      <dt>{label}</dt>
      <dd>{formatTokens(usage.Input + usage.CacheRead + usage.CacheWrite)}</dd>
      <dt>In</dt>
      <dd>{formatTokens(usage.Input)}</dd>
      <dt>Cache read</dt>
      <dd>{formatTokens(usage.CacheRead)}</dd>
      <dt>Cache write</dt>
      <dd>{formatTokens(usage.CacheWrite)}</dd>
      <dt>Output</dt>
      <dd>{formatTokens(usage.Output)}</dd>
    </dl>
  );
}

function FallbackNotice({ fallback }: { fallback: ModelFallback }) {
  return (
    <div className="msg-fallback" role="note">
      <span className="fallback-label">{fallbackNoticeLabel(fallback)}</span>
    </div>
  );
}

function AttachmentTile({
  attachment,
  blobBase,
}: {
  attachment: Attachment;
  blobBase: string;
}) {
  const url = `${blobBase}/${attachment.SHA256}`;
  if (isImageMedia(attachment.MediaType)) {
    return (
      <figure className="attachment image">
        <a href={url} target="_blank" rel="noreferrer">
          <img
            src={url}
            alt={attachment.Filename || "attachment"}
            loading="lazy"
          />
        </a>
        <figcaption className="muted small">
          {attachment.Filename ? `${attachment.Filename} · ` : ""}
          {chipLabel(attachment.MediaType, attachment.ByteLen)}
        </figcaption>
      </figure>
    );
  }
  return (
    <a
      className="attachment file stamp"
      href={url}
      target="_blank"
      rel="noreferrer"
    >
      {attachment.Filename ? `${attachment.Filename} · ` : ""}
      {chipLabel(attachment.MediaType, attachment.ByteLen)}
    </a>
  );
}

function ToolChip({ tool, blobBase }: { tool: ToolCall; blobBase: string }) {
  const path = toolFilePath(tool);
  return (
    <div className="tool-chip">
      <span className="tname">{tool.ToolName}</span>
      {path ? (
        <span className="tpath muted" title={toolFileTitle(tool)}>
          {path}
        </span>
      ) : null}
      {tool.Detail ? (
        <span className="tdetail muted" title={tool.Detail}>
          {detailLabel(tool.Detail)}
        </span>
      ) : null}
      {tool.InputSHA ? (
        <button
          type="button"
          className="stamp body-toggle"
          onClick={() => openToolInspector(tool, blobBase, "input")}
        >
          in: {chipLabel(tool.InputMediaType, tool.InputBytes)}
        </button>
      ) : tool.InputBytes > 0 ? (
        <span className="stamp">
          in: {chipLabel(tool.InputMediaType, tool.InputBytes)}
        </span>
      ) : null}
      {tool.ResultStatus ? (
        <span
          className={`tstatus ${tool.ResultStatus === "error" ? "err" : "ok"}`}
        >
          {tool.ResultStatus}
        </span>
      ) : null}
      {tool.ResultSHA ? (
        <button
          type="button"
          className="stamp body-toggle"
          onClick={() => openToolInspector(tool, blobBase, "result")}
        >
          out: {chipLabel(tool.ResultMediaType, tool.ResultBytes)}
        </button>
      ) : tool.ResultBytes > 0 ? (
        <span className="stamp">
          out: {chipLabel(tool.ResultMediaType, tool.ResultBytes)}
        </span>
      ) : null}
    </div>
  );
}
