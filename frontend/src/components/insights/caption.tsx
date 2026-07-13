import type { ReactNode } from "react";

// InstrumentCaption is every instrument's closing two-register caption: a
// plain-language lead sentence, then a demoted "How it's measured" disclosure
// carrying the exact table/column/predicate. The lead and disclosure text are
// quoted verbatim from the pre-React insights.templ; only the wrapper is new.
export function InstrumentCaption({
  lead,
  children,
}: {
  lead: string;
  children: ReactNode;
}) {
  return (
    <>
      <p className="panel-caption">{lead}</p>
      <details className="panel-how">
        <summary>How it's measured</summary>
        <div className="panel-how-body">{children}</div>
      </details>
    </>
  );
}
