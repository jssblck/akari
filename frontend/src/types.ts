export type Viewer = {
  authenticated: boolean;
  user_id?: number;
  username?: string;
  is_admin: boolean;
  overview_public: boolean;
  csrf_token?: string;
};

export type DateRange = { Key: string; Label: string; Days: number };

export type DayPoint = {
  Day: string;
  CostUSD: number;
  Input: number;
  Output: number;
  CacheRead: number;
  CacheWrite: number;
};

export type Breakdown = {
  Label: string;
  CostUSD: number;
  Input: number;
  Output: number;
  CacheRead: number;
  CacheWrite: number;
  Reasoning: number;
  Sessions: number;
  CostIncomplete: boolean;
};

export type Analytics = {
  Series: DayPoint[] | null;
  Models: Breakdown[] | null;
  Agents: Breakdown[] | null;
  Users: Breakdown[] | null;
  TotalCost: number;
  TotalIn: number;
  TotalOut: number;
  TotalCacheRead: number;
  TotalCacheWrite: number;
  TotalReasoning: number;
  Sessions: number;
  CostIncomplete: boolean;
  Cache: {
    Input: number;
    Output: number;
    CacheRead: number;
    CacheWrite: number;
    SavingsUSD: number;
    SavingsIncomplete: boolean;
  };
};

export type User = { ID: number; Username: string; IsAdmin: boolean };

export type Project = {
  ID: number;
  RemoteKey: string;
  Host: string;
  Owner: string;
  Repo: string;
  DisplayName: string;
  Kind: string;
  SessionCount: number;
  TotalCostUSD: number;
  TotalInput: number;
  TotalOutput: number;
  TotalCacheRead: number;
  TotalCacheWrite: number;
  CostIncomplete: boolean;
  LastActivity: string | null;
  OverviewPublic: boolean;
};

export type SessionSummary = {
  ID: number;
  Agent: string;
  Machine: string;
  GitBranch: string;
  Username: string;
  MessageCount: number;
  UserMessageCount: number;
  ModelFallbackCount: number;
  TotalInput: number;
  TotalOutput: number;
  TotalCacheWrite: number;
  TotalCacheRead: number;
  TotalCostUSD: number;
  CostIncomplete: boolean;
  Visibility: string;
  PublicID: string | null;
  StartedAt: string | null;
  EndedAt: string | null;
  LastActiveAt: string | null;
  Title: string;
};

export type SessionRow = SessionSummary & {
  ProjectID: number;
  ProjectKey: string;
  ProjectName: string;
  ProjectKind: string;
  Grade: string | null;
  Outcome: string;
  Search: { Text: string; MatchStart: number; MatchEnd: number };
  Tree: {
    SubagentCount: number;
    TotalCostUSD: number;
    CostIncomplete: boolean;
  };
};

export type SessionDetail = SessionSummary & {
  OwnerID: number;
  ProjectID: number;
  ProjectKey: string;
  ProjectName: string;
  ProjectKind: string;
  Cwd: string;
  ParentID: number | null;
  TotalCacheSavingsUSD: number;
  CacheSavingsIncomplete: boolean;
};

export type TurnUsage = {
  Input: number;
  Output: number;
  CacheRead: number;
  CacheWrite: number;
  Reasoning: number;
  CostUSD: number;
  CostIncomplete: boolean;
};

export type Message = {
  Ordinal: number;
  Role: string;
  Content: string;
  ThinkingText: string;
  Model: string;
  HasThinking: boolean;
  HasToolUse: boolean;
  ThinkingBytes: number;
  Timestamp: string | null;
  Usage: TurnUsage | null;
  DuplicatePrompt: boolean;
};

export type ToolCall = {
  MessageOrdinal: number;
  CallIndex: number;
  ToolName: string;
  Category: string;
  FilePath: string;
  FileRelPath: string;
  Detail: string;
  InputSHA: string;
  InputBytes: number;
  InputMediaType: string;
  ResultSHA: string;
  ResultBytes: number;
  ResultMediaType: string;
  ResultStatus: string;
};

export type Attachment = {
  MessageOrdinal: number;
  SHA256: string;
  MediaType: string;
  ByteLen: number;
  Filename: string;
};

export type TranscriptPage = {
  Msgs: Message[] | null;
  Seed: Message[] | null;
  Tools: ToolCall[] | null;
  Attachments: Attachment[] | null;
  Fallbacks: unknown[] | null;
  HasEarlier: boolean;
  EarlierCount: number;
  More: boolean;
};

export type LabeledCount = { Key: string; Count: number };

export type Insights = {
  Quality: {
    Grades: LabeledCount[];
    Outcomes: LabeledCount[];
    Sessions: number;
    Graded: number;
  };
  Archetypes: LabeledCount[];
  Concurrency: Record<string, number>;
  Velocity: Record<string, number>;
  Tools: Record<string, unknown>;
  Hygiene: Record<string, number>;
  Churn: Record<string, unknown>;
  Context: Record<string, number>;
  Trends: Record<string, unknown> | null;
};

export type SessionSnapshot = {
  Audit: {
    Detail: SessionDetail;
    Signals: Record<string, unknown>;
    Subagents: Array<
      SessionSummary & { Grade: string | null; Outcome: string }
    > | null;
    Fallbacks: unknown[] | null;
  };
  Page: TranscriptPage;
  Outline: Message[] | null;
  Tools: ToolCall[] | null;
  DupIDs: number;
};

export type PublicSessionSnapshot = {
  Audit: SessionSnapshot["Audit"];
  Page: TranscriptPage;
  Outline: Message[] | null;
  Tools: ToolCall[] | null;
  ProjectionRevision: number;
};

export type APIError = { error: string };
