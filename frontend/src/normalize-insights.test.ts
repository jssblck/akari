import { describe, expect, it } from "vitest";

import { normalizeInsights } from "./normalize-insights";

describe("normalizeInsights", () => {
  it("turns nil analytics slices into empty chart series", () => {
    const wire = JSON.parse(`{
      "Quality":{"Grades":null,"Outcomes":null},
      "Archetypes":null,
      "Tools":{"Tools":null},
      "Churn":{"Files":null},
      "Trends":{
        "BucketStarts":null,"Labels":null,
        "FleetMix":{"Models":null},
        "Gallery":{"Rows":null},
        "Velocity":{"ActiveHours":null,"WallHours":null,"ResponseP50":null,"ResponseP90":null,"ResponseP99":null,"MsgsPerMin":null,"ToolsPerMin":null},
        "Tools":{"Reliability":null,"MixOrder":null,"Mix":[null],"FailFleet":null,"FailWorst":[]},
        "Churn":{"ReEdits":null,"Files":null,"Tree":null},
        "Signals":{"GradeShare":[null],"GPA":null,"ArchetypeShare":null,"CompletedRate":null,"AbandonedRate":null,"OutcomeTotal":null,"CompletedCount":null,"AbandonedCount":null,"HygieneTerse":null,"HygieneRepeated":null,"HygieneNoCode":null,"HygieneUnstructured":null,"ContextResets":null,"ContextHistogram":null,"ContextMarkers":null},
        "Economics":{"CostCompleted":null,"CostAbandoned":null,"CostOther":null,"CacheSavings":null,"CacheHitRate":null,"CacheMeasured":null},
        "Subagents":{"DelegateShare":null,"CostShare":null,"FanoutOrder":null,"FanoutRows":[null]},
        "Rhythm":{"Cells":[null]}
      }
    }`);

    const normalized = normalizeInsights(wire);

    expect(normalized.Archetypes).toEqual([]);
    expect(normalized.Quality.Grades).toEqual([]);
    expect(normalized.Tools.Tools).toEqual([]);
    expect(normalized.Trends?.Tools.Mix).toEqual([{}]);
    expect(normalized.Trends?.Signals.GradeShare).toEqual([{}]);
    expect(normalized.Trends?.Subagents.FanoutRows).toEqual([{}]);
    expect(normalized.Trends?.Rhythm.Cells).toEqual([[]]);
  });
});
