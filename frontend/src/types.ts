export type Viewer = {
  authenticated: boolean;
  user_id?: number;
  username?: string;
  is_admin: boolean;
  overview_public: boolean;
  csrf_token?: string;
  version: string;
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

export type QualityDistribution = {
  Grades: LabeledCount[]; // canonical order A,B,C,D,F,"" (unscored)
  Outcomes: LabeledCount[]; // canonical order completed, errored, abandoned, unknown
  Sessions: number;
  Graded: number;
};

export type ConcurrencyStats = {
  FleetPeak: number;
  FleetPeakAt: string;
  BusiestUser: string;
  BusiestUserPeak: number;
  AvgConcurrent: number;
  Sessions: number;
};

export type VelocityStats = {
  // Duration fields arrive as nanoseconds (Go time.Duration -> JSON number);
  // divide by 1e9 for seconds.
  ResponseP50: number;
  ResponseP90: number;
  FirstResponseP50: number;
  MsgsPerActiveMin: number;
  ToolsPerActiveMin: number;
  ActiveSeconds: number;
  Turns: number;
  Sessions: number;
};

export type ToolStat = { Name: string; Calls: number; Failures: number };

export type ToolStats = {
  TotalCalls: number;
  TotalFailures: number;
  Turns: number;
  Tools: ToolStat[]; // busiest first, cap 10
  Clipped: number;
};

export type PromptHygiene = {
  Prompts: number;
  Short: number;
  Duplicate: number;
  NoCodeContext: number;
  Sessions: number;
  UnstructuredStarts: number;
};

export type ChurnFile = {
  ProjectID: number;
  Project: string;
  Path: string;
  Edits: number;
  Sessions: number;
};

// FileChurn is the window-wide top-10 churned-file list, distinct from
// Trends.Churn.Tree (the treemap's per-bucket, project/folder/file tree).
export type FileChurn = { Files: ChurnFile[]; Clipped: number };

export type ContextHealthStats = {
  Sessions: number;
  PeakTokensP50: number;
  PeakTokensP90: number;
  PeakTokensMax: number;
  TotalResets: number;
  SessionsWithReset: number;
};

export type ModelSeries = {
  Model: string;
  Share: number[]; // percent of bucket i's total tokens
  First: number; // first bucket index this model appears in, -1 if never
  WindowShare: number; // whole-window token share percent
};

export type FleetMix = {
  Models: ModelSeries[]; // tokens desc, "other" fold last
  NewestModel: string; // "" when nothing arrived in window
  NewestFirst: number;
};

export type ContextBucket = { Lo: number; Hi: number; Count: number };
export type ContextMarker = { Tokens: number; Kind: "p50" | "p90" | "max" };

export type SignalTrends = {
  GradeShare: Record<string, number>[]; // key A/B/C/D/F/"" -> percent, per bucket
  GPA: number[]; // 0..4 per bucket
  ArchetypeShare: Record<string, number>[]; // quick/standard/deep/marathon/automation -> percent
  CompletedRate: number[];
  AbandonedRate: number[];
  OutcomeTotal: number[];
  CompletedCount: number[];
  AbandonedCount: number[];
  HygieneTerse: number[];
  HygieneRepeated: number[];
  HygieneNoCode: number[];
  HygieneUnstructured: number[];
  ContextResets: number[];
  ContextHistogram: ContextBucket[]; // window-wide, not per-bucket
  ContextMarkers: ContextMarker[]; // window-wide annotations
};

export type Economics = {
  CostCompleted: number[];
  CostAbandoned: number[];
  CostOther: number[];
  CacheSavings: number[];
  CacheHitRate: number[];
  CacheMeasured: boolean[];
  TotalSpend: number;
  TotalAbandoned: number;
  AbandonedSharePct: number;
  TotalCacheSavings: number;
  CacheHitRateLatest: number;
  CostIncomplete: boolean;
  AbandonedIncomplete: boolean;
  CacheSavingsIncomplete: boolean;
};

export type VelocityTrends = {
  ActiveHours: number[];
  WallHours: number[];
  // Seconds (float), unlike Insights.Velocity's nanosecond Duration fields.
  ResponseP50: number[];
  ResponseP90: number[];
  ResponseP99: number[];
  MsgsPerMin: number[];
  ToolsPerMin: number[];
};

export type ToolPoint = {
  Name: string;
  Calls: number;
  Failures: number;
  Sessions: number;
  Category: string;
};

export type ToolFailSeries = { Name: string; Rate: number[] };

export type ToolTrends = {
  Reliability: ToolPoint[]; // whole-window snapshot, not bucketed, cap 60
  MixOrder: string[]; // category keys, busiest first, "other" last, cap 6
  Mix: Record<string, number>[]; // per bucket: category -> percent
  FailFleet: number[]; // fleet error rate percent per bucket
  FailWorst: ToolFailSeries[]; // top-3 worst tools by failure count
};

export type ChurnNode = {
  Project: string;
  Folder: string;
  Path: string;
  Edits: number;
  Sessions: number;
};

export type ChurnTrend = {
  ReEdits: number[]; // hot-file re-edits per bucket
  Files: number[]; // hot files per bucket
  Tree: ChurnNode[]; // cap 150, busiest first
  Clipped: number;
  TotalReEdits: number;
  TotalHotFiles: number;
  Projects: number; // uncapped distinct-project count; drives sole-project detection
};

export type GallerySession = {
  DurationS: number;
  CostUSD: number;
  CostIncomplete: boolean;
  Archetype: string;
  Grade: string;
  Outcome: string;
};

export type Gallery = {
  Rows: GallerySession[]; // cap 400 most recent
  Total: number; // full cohort; figures cover the full cohort, not just Rows
  MedianDurationS: number;
  MedianCostUSD: number;
  MedianCompletedCostUSD: number;
  PriciestDurationS: number;
  PriciestCostUSD: number;
  LongestDurationS: number;
  LongestCostUSD: number;
  CostIncomplete: boolean;
};

export type RhythmGrid = { Cells: number[][] }; // Cells[dow][hour], dow 0=Mon..6=Sun, UTC hour

export type SubagentStats = {
  DelegateShare: number[];
  CostShare: number[];
  FanoutOrder: ["one", "twoThree", "fourSeven", "eightPlus"];
  FanoutRows: Record<string, number>[];
  SessionsThatDelegatePct: number;
  SubagentSessionsInWindow: number;
  CostThroughSubagentsPct: number;
  DeepestTree: number;
  CostShareIncomplete: boolean;
};

export type Trends = {
  Unit: "day" | "week";
  BucketStarts: string[]; // RFC3339, oldest first: the x axis
  Labels: string[]; // pre-formatted bucket labels aligned to BucketStarts
  FleetMix: FleetMix;
  Gallery: Gallery;
  Velocity: VelocityTrends;
  Tools: ToolTrends;
  Churn: ChurnTrend;
  Signals: SignalTrends;
  Economics: Economics;
  Subagents: SubagentStats;
  Rhythm: RhythmGrid;
};

export type Insights = {
  Quality: QualityDistribution;
  Archetypes: LabeledCount[];
  Concurrency: ConcurrencyStats;
  Velocity: VelocityStats;
  Tools: ToolStats;
  Hygiene: PromptHygiene;
  Churn: FileChurn;
  Context: ContextHealthStats;
  Trends: Trends | null;
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

export type APIError = { error: string; code?: string };
