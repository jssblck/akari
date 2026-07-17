import type { components } from "./api.generated";

type Schema = components["schemas"];

type ArrayMember<T> = Extract<NonNullable<T>, readonly unknown[]>;
type NormalizeArrays<T> = [ArrayMember<T>] extends [never]
  ? T extends object
    ? { [Key in keyof T]: NormalizeArrays<T[Key]> }
    : T
  : ArrayMember<T> extends readonly (infer Item)[]
    ? NormalizeArrays<NonNullable<Item>>[]
    : never;

export type Viewer = Schema["Viewer"];
export type DateRange = Schema["DateRange"];
export type DayPoint = Schema["DayPoint"];
export type Breakdown = Schema["Breakdown"];
export type Analytics = Schema["Analytics"];
export type User = Schema["OverviewUser"];
export type Project = Schema["ProjectSummary"];
export type SessionSummary = Schema["SessionSummary"];
export type SessionRow = Schema["SessionRow"];
export type SessionDetail = Schema["SessionDetail"];
export type TurnUsage = Schema["TurnUsage"];
export type Message = Schema["Message"];
export type ToolCall = Schema["ToolCallView"];
export type Attachment = Schema["AttachmentView"];
export type TranscriptPage = Schema["TranscriptPage"];
export type LabeledCount = Schema["LabeledCount"];
export type Insights = NormalizeArrays<Schema["Insights"]>;
export type SessionSnapshot = Schema["SessionSnapshot"];
export type PublicSessionSnapshot = Schema["PublicSessionSnapshot"];
export type APIError = Schema["Error"];

export type QualityDistribution = NormalizeArrays<
  Schema["QualityDistribution"]
>;
export type ConcurrencyStats = NormalizeArrays<Schema["ConcurrencyStats"]>;
export type VelocityStats = NormalizeArrays<Schema["VelocityStats"]>;
export type ToolStat = NormalizeArrays<Schema["ToolStat"]>;
export type ToolStats = NormalizeArrays<Schema["ToolStats"]>;
export type PromptHygiene = NormalizeArrays<Schema["PromptHygiene"]>;
export type ChurnFile = NormalizeArrays<Schema["ChurnFile"]>;
export type FileChurn = NormalizeArrays<Schema["FileChurn"]>;
export type ContextHealthStats = NormalizeArrays<Schema["ContextHealthStats"]>;
export type ModelSeries = NormalizeArrays<Schema["ModelSeries"]>;
export type FleetMix = NormalizeArrays<Schema["FleetMix"]>;
export type ContextBucket = NormalizeArrays<Schema["ContextBucket"]>;
export type ContextMarker = NormalizeArrays<Schema["ContextMarker"]>;
export type SignalTrends = NormalizeArrays<Schema["SignalTrends"]>;
export type Economics = NormalizeArrays<Schema["Economics"]>;
export type VelocityTrends = NormalizeArrays<Schema["VelocityTrends"]>;
export type ToolPoint = NormalizeArrays<Schema["ToolPoint"]>;
export type ToolFailSeries = NormalizeArrays<Schema["ToolFailSeries"]>;
export type ToolTrends = NormalizeArrays<Schema["ToolTrends"]>;
export type ChurnNode = NormalizeArrays<Schema["ChurnNode"]>;
export type ChurnTrend = NormalizeArrays<Schema["ChurnTrend"]>;
export type GallerySession = NormalizeArrays<Schema["GallerySession"]>;
export type Gallery = NormalizeArrays<Schema["Gallery"]>;
export type RhythmGrid = NormalizeArrays<Schema["RhythmGrid"]>;
export type SubagentStats = NormalizeArrays<Schema["SubagentStats"]>;
export type Trends = NormalizeArrays<Schema["Trends"]>;

export type Token = Schema["AccountToken"];
export type Connection = Schema["OAuthGrant"];
export type Invite = Schema["AccountInvite"];
export type Chapter = Schema["Chapter"];
export type Heading = Schema["Heading"];
export type FacetCount = Schema["FacetCount"];
export type ProjectFacet = Schema["ProjectFacet"];

export type AccountResponse = Schema["AccountResponse"];
export type AccountProject = Schema["AccountProject"];
export type OverviewResponse = Schema["OverviewResponse"];
export type InsightsResponse = Schema["InsightsResponse"];
export type ProjectsResponse = Schema["ProjectsResponse"];
export type ProjectResponse = Schema["ProjectResponse"];
export type SessionsResponse = Schema["SessionsResponse"];
export type SessionResponse = Schema["SessionResponse"];
export type TranscriptResponse = Schema["TranscriptResponse"];
export type PublicOverviewResponse = Schema["PublicOverviewResponse"];
export type PublicProjectResponse = Schema["PublicProjectResponse"];
export type PublicSessionResponse = Schema["PublicSessionResponse"];
export type GuideResponse = Schema["GuideResponse"];
export type OAuthConsentResponse = Schema["OAuthConsentResponse"];
export type CreatedTokenResponse = Schema["CreatedTokenResponse"];
export type CreatedInviteResponse = Schema["CreatedInviteResponse"];
export type DeletedSessionResponse = Schema["DeletedSessionResponse"];
export type PublicationResponse = Schema["PublicationResponse"];
export type SessionPublicationResponse = Schema["SessionPublicationResponse"];
