// Local type augmentations for the session detail view. The server's Go structs
// carry more fields than the shared frontend/src/types.ts declares (that file is
// owned by another agent), so the fields the session page needs but the shared
// type omits are declared here and applied with a cast at the read site. Every
// field name matches the Go struct's JSON encoding (the field name verbatim: none
// of these structs carry `json` tags), so no server change is needed to read them.

import type { Message, ToolCall, TurnUsage } from "../types";

// TurnUsage as the session page needs it: the shared type omits ContextTokens (a
// field only the transcript's per-turn stamp and breakdown card use).
export type TurnUsageFull = TurnUsage & { ContextTokens: number };

// Message as the session page needs it: the shared type omits the prompt-hygiene
// facts (meaningful only when PromptFactsCurrent is true) and carries a narrower
// Usage. asFullMessage() is the single cast site so a mismatch surfaces there
// rather than at every read.
export type MessageFull = Omit<Message, "Usage"> & {
  PromptShort: boolean;
  PromptNoCode: boolean;
  PromptFactsCurrent: boolean;
  Usage: TurnUsageFull | null;
};

export function asFullMessage(m: Message): MessageFull {
  return m as unknown as MessageFull;
}

// store.SessionSignals, mirrored field for field. Score, Grade, and the
// context/thinking figures are Go pointer fields (nil when unmeasured), which
// JSON renders as null.
export type SessionSignals = {
  SessionID: number;
  Outcome: string;
  OutcomeConfidence: string;
  Score: number | null;
  Grade: string | null;
  ToolCalls: number;
  ToolFailures: number;
  ToolRetries: number;
  EditChurn: number;
  LongestFailureStreak: number;
  PromptCount: number;
  ShortPromptCount: number;
  DuplicatePromptCount: number;
  NoCodeContextCount: number;
  UnstructuredStart: boolean;
  PeakContextTokens: number | null;
  ContextResetCount: number | null;
  AssistantTurns: number | null;
  ThinkingTurns: number | null;
  ThinkingTailTokens: number | null;
  ThinkingPeakTokens: number | null;
};

export function asSessionSignals(s: Record<string, unknown>): SessionSignals {
  return s as unknown as SessionSignals;
}

// store.ModelFallback, mirrored field for field.
export type ModelFallback = {
  MessageOrdinal: number | null;
  FromModel: string;
  ToModel: string;
  Trigger: string;
  RefusalCategory: string;
  RefusalExplanation: string;
  DeclinedInput: number | null;
  DeclinedOutput: number | null;
  DeclinedCacheWrite: number | null;
  DeclinedCacheRead: number | null;
  OccurredAt: string | null;
  DedupKey: string;
};

export function asFallbacks(fbs: unknown[] | null): ModelFallback[] {
  return (fbs ?? []) as unknown as ModelFallback[];
}

// ToolCall already matches store.ToolCallView field for field; this alias just
// names the intent at call sites that treat a tool call as the outline/inspector
// input rather than a transcript-window row.
export type ToolCallFull = ToolCall;
