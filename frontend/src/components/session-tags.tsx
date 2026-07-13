// Small state chips shared by the sessions feed and the session detail header, so
// a project kind, a public link, a grade, a fallback count, and a subagent fan-out
// read identically everywhere they appear. Ported from web.kindTag,
// web.sessionPublicTag, and the feed row's inline chips (sessions.templ).
import { ArrowSquareOutIcon } from "@phosphor-icons/react";

import { formatCost } from "../format";
import {
  fallbackBadgeLabel,
  fallbackBadgeTitle,
  rowOutcomeNote,
} from "./session-quality";

// web.kindTag: a chip for a non-remote project kind; a remote project gets none,
// since it is the default and needs no label.
export function KindTag({ kind }: { kind: string }) {
  if (kind === "standalone")
    return (
      <span
        className="tag standalone"
        title="No .git, or no git origin remote."
      >
        standalone
      </span>
    );
  if (kind === "orphaned")
    return (
      <span
        className="tag orphaned"
        title="The working directory no longer exists on disk."
      >
        orphaned
      </span>
    );
  return null;
}

// web.sessionPublicTag: nothing while private, a linked chip once a public id is
// known, else the plain marker.
export function SessionPublicTag({
  visibility,
  publicID,
  linked = true,
}: {
  visibility: string;
  publicID: string | null;
  // linked=false renders the plain marker even when a public id is known, for a
  // context that is itself already a link (a feed row, a subagent table row): an
  // anchor nested inside another anchor is invalid HTML and unreliable to click,
  // so a row-level context suppresses the nested link and keeps just the badge.
  // The full linked chip still reads on the session detail header, which is not
  // nested inside a bigger clickable target.
  linked?: boolean;
}) {
  if (visibility !== "public") return null;
  if (!publicID || !linked) return <span className="tag public">public</span>;
  return (
    <a
      className="tag public tag-link"
      href={`/s/${publicID}`}
      target="_blank"
      rel="noreferrer"
      title="Open the public page in a new tab"
    >
      public <ArrowSquareOutIcon size={11} />
    </a>
  );
}

// web.RowGradeClass + the feed/session grade chip: a letter reading in the
// report-card palette already defined in styles.css (.tag.grade-a .. .grade-f).
export function GradeTag({ grade }: { grade: string | null }) {
  if (!grade) return null;
  return (
    <span
      className={`tag grade grade-${grade.toLowerCase()}`}
      title="quality grade"
    >
      {grade}
    </span>
  );
}

// web.RowOutcomeNote: a short outcome word worth a glance (abandoned, errored);
// completed and unknown stay quiet.
export function OutcomeTag({ outcome }: { outcome: string }) {
  const note = rowOutcomeNote(outcome);
  if (!note) return null;
  return (
    <span className={`status ${outcome}`} title="session outcome">
      {note}
    </span>
  );
}

// web.FallbackBadgeLabel / FallbackBadgeTitle: the feed row's model-fallback count.
export function FallbackTag({ count }: { count: number }) {
  if (count <= 0) return null;
  return (
    <span className="tag warn" title={fallbackBadgeTitle(count)}>
      {fallbackBadgeLabel(count)}
    </span>
  );
}

// web.FanoutLabel / FanoutTitle: the subtree's subagent count and its whole-work-item cost.
export function FanoutTag({
  subagentCount,
  costUSD,
  costIncomplete,
}: {
  subagentCount: number;
  costUSD: number;
  costIncomplete: boolean;
}) {
  if (subagentCount <= 0) return null;
  const unit = subagentCount === 1 ? "subagent" : "subagents";
  return (
    <span
      className="tag fanout"
      title={`Whole work item: ${formatCost(costUSD, costIncomplete)} across ${subagentCount} ${unit} fanned out (the row's own cost is the root turn's alone)`}
    >
      {subagentCount} {unit} · {formatCost(costUSD, costIncomplete)}
    </span>
  );
}
