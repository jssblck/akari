import type { components } from "./api.generated";
import type { Insights } from "./types";

type WireInsights = components["schemas"]["Insights"];

// The analytics store leaves slices nil when a cohort has no rows. Normalize
// those wire-level nulls once so chart components can operate on empty series
// without each visualization inventing its own fallback rules.
export function normalizeInsights(value: WireInsights): Insights {
  const trends = value.Trends;
  return {
    ...value,
    Archetypes: value.Archetypes ?? [],
    Quality: {
      ...value.Quality,
      Grades: value.Quality.Grades ?? [],
      Outcomes: value.Quality.Outcomes ?? [],
    },
    Tools: { ...value.Tools, Tools: value.Tools.Tools ?? [] },
    Churn: { ...value.Churn, Files: value.Churn.Files ?? [] },
    Trends: trends
      ? {
          ...trends,
          BucketStarts: trends.BucketStarts ?? [],
          Labels: trends.Labels ?? [],
          FleetMix: {
            ...trends.FleetMix,
            Models: (trends.FleetMix.Models ?? []).map((model) => ({
              ...model,
              Share: model.Share ?? [],
            })),
          },
          Gallery: {
            ...trends.Gallery,
            Rows: trends.Gallery.Rows ?? [],
          },
          Velocity: {
            ...trends.Velocity,
            ActiveHours: trends.Velocity.ActiveHours ?? [],
            WallHours: trends.Velocity.WallHours ?? [],
            ResponseP50: trends.Velocity.ResponseP50 ?? [],
            ResponseP90: trends.Velocity.ResponseP90 ?? [],
            ResponseP99: trends.Velocity.ResponseP99 ?? [],
            MsgsPerMin: trends.Velocity.MsgsPerMin ?? [],
            ToolsPerMin: trends.Velocity.ToolsPerMin ?? [],
          },
          Tools: {
            ...trends.Tools,
            Reliability: trends.Tools.Reliability ?? [],
            MixOrder: trends.Tools.MixOrder ?? [],
            Mix: (trends.Tools.Mix ?? []).map((row) => row ?? {}),
            FailFleet: trends.Tools.FailFleet ?? [],
            FailWorst: (trends.Tools.FailWorst ?? []).map((tool) => ({
              ...tool,
              Rate: tool.Rate ?? [],
            })),
          },
          Churn: {
            ...trends.Churn,
            ReEdits: trends.Churn.ReEdits ?? [],
            Files: trends.Churn.Files ?? [],
            Tree: trends.Churn.Tree ?? [],
          },
          Signals: {
            ...trends.Signals,
            GradeShare: (trends.Signals.GradeShare ?? []).map(
              (row) => row ?? {},
            ),
            GPA: trends.Signals.GPA ?? [],
            ArchetypeShare: (trends.Signals.ArchetypeShare ?? []).map(
              (row) => row ?? {},
            ),
            CompletedRate: trends.Signals.CompletedRate ?? [],
            AbandonedRate: trends.Signals.AbandonedRate ?? [],
            OutcomeTotal: trends.Signals.OutcomeTotal ?? [],
            CompletedCount: trends.Signals.CompletedCount ?? [],
            AbandonedCount: trends.Signals.AbandonedCount ?? [],
            HygieneTerse: trends.Signals.HygieneTerse ?? [],
            HygieneRepeated: trends.Signals.HygieneRepeated ?? [],
            HygieneNoCode: trends.Signals.HygieneNoCode ?? [],
            HygieneUnstructured: trends.Signals.HygieneUnstructured ?? [],
            ContextResets: trends.Signals.ContextResets ?? [],
            ContextHistogram: trends.Signals.ContextHistogram ?? [],
            ContextMarkers: trends.Signals.ContextMarkers ?? [],
          },
          Economics: {
            ...trends.Economics,
            CostCompleted: trends.Economics.CostCompleted ?? [],
            CostAbandoned: trends.Economics.CostAbandoned ?? [],
            CostOther: trends.Economics.CostOther ?? [],
            CacheSavings: trends.Economics.CacheSavings ?? [],
            CacheHitRate: trends.Economics.CacheHitRate ?? [],
            CacheMeasured: trends.Economics.CacheMeasured ?? [],
          },
          Subagents: {
            ...trends.Subagents,
            DelegateShare: trends.Subagents.DelegateShare ?? [],
            CostShare: trends.Subagents.CostShare ?? [],
            FanoutOrder: trends.Subagents.FanoutOrder ?? [],
            FanoutRows: (trends.Subagents.FanoutRows ?? []).map(
              (row) => row ?? {},
            ),
          },
          Rhythm: {
            ...trends.Rhythm,
            Cells: (trends.Rhythm.Cells ?? []).map((row) => row ?? []),
          },
        }
      : null,
  };
}
