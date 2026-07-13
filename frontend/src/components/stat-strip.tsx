import type { ReactNode } from "react";

export function StatStrip({ children }: { children: ReactNode }) {
  return <div className="stat-strip">{children}</div>;
}

export function Stat({
  label,
  value,
  note,
}: {
  label: string;
  value: ReactNode;
  note?: string;
}) {
  return (
    <div className="stat">
      <span className="label">{label}</span>
      <strong>{value}</strong>
      {note ? <span className="stat-note">{note}</span> : null}
    </div>
  );
}
