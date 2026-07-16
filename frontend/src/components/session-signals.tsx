import {
  CheckCircleIcon,
  QuestionIcon,
  WarningDiamondIcon,
  XCircleIcon,
} from "@phosphor-icons/react";

import { HoverTip } from "./token-card";
import "./session-signals.css";

const GRADE_DETAILS: Record<string, string> = {
  A: "Strong execution with few or no observed quality problems.",
  B: "Good execution with minor quality deductions.",
  C: "Mixed execution with meaningful quality deductions.",
  D: "Weak execution with substantial quality problems.",
  F: "Failed execution with severe quality problems.",
};

const OUTCOME_LABELS: Record<string, string> = {
  completed: "Completed",
  errored: "Errored",
  abandoned: "Abandoned",
  unknown: "Unknown",
};

const OUTCOME_DETAILS: Record<string, string> = {
  completed: "The session ended normally.",
  errored: "The session ended after an execution error.",
  abandoned: "The transcript stopped without a normal completion signal.",
  unknown: "No reliable terminal outcome was detected.",
};

export function SessionGrade({ grade }: { grade: string | null }) {
  const label = grade ?? "-";
  return (
    <HoverTip
      className="srow-signal srow-grade"
      summary={
        <span
          className={
            grade ? `tag grade grade-${grade.toLowerCase()}` : "signal-empty"
          }
        >
          {label}
        </span>
      }
    >
      <strong className="tip-title">
        {grade ? `Quality grade ${grade}` : "Not graded"}
      </strong>
      <p className="tip-copy">
        {grade
          ? (GRADE_DETAILS[grade] ?? "Akari assigned this quality grade.")
          : "A session is graded after it settles."}
      </p>
    </HoverTip>
  );
}

export function SessionOutcome({ outcome }: { outcome: string }) {
  const label = OUTCOME_LABELS[outcome] ?? outcome;
  const iconProps = { size: 17, weight: "bold" as const, "aria-hidden": true };
  let icon = <QuestionIcon {...iconProps} />;
  if (outcome === "completed") icon = <CheckCircleIcon {...iconProps} />;
  if (outcome === "abandoned") icon = <WarningDiamondIcon {...iconProps} />;
  if (outcome === "errored") icon = <XCircleIcon {...iconProps} />;
  return (
    <HoverTip
      className={`srow-signal srow-outcome outcome-${outcome}`}
      summary={
        <>
          {icon}
          <span className="sr-only">{label}</span>
        </>
      }
    >
      <strong className="tip-title">{label}</strong>
      <p className="tip-copy">
        {OUTCOME_DETAILS[outcome] ?? OUTCOME_DETAILS.unknown}
      </p>
    </HoverTip>
  );
}
