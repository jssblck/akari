import { useSearchParams } from "react-router-dom";

import type { DateRange } from "../types";

export function RangeTabs({
  ranges,
  active,
}: {
  ranges: DateRange[];
  active: string;
}) {
  const [params, setParams] = useSearchParams();
  return (
    <fieldset className="segmented">
      <legend className="sr-only">Trailing window</legend>
      {ranges.map((range) => (
        <button
          type="button"
          className={range.Key === active ? "active" : ""}
          aria-pressed={range.Key === active}
          key={range.Key}
          onClick={() => {
            const next = new URLSearchParams(params);
            next.set("range", range.Key);
            setParams(next);
          }}
        >
          {range.Label}
        </button>
      ))}
    </fieldset>
  );
}
