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
  // The URL param wins over the server-echoed range so the active tab flips
  // on click; `active` covers the default view with no explicit param.
  const current = params.get("range") ?? active;
  return (
    <fieldset className="segmented">
      <legend className="sr-only">Trailing window</legend>
      {ranges.map((range) => (
        <button
          type="button"
          className={range.Key === current ? "active" : ""}
          aria-pressed={range.Key === current}
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
