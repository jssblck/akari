export type LegendItem = { color: string; label: string; title?: string };

export function Legend({ items }: { items: LegendItem[] }) {
  if (!items.length) return null;
  return (
    <ul className="legend">
      {items.map((it) => (
        <li className="legend-chip" key={it.label} title={it.title}>
          <span className="legend-swatch" style={{ background: it.color }} />
          {it.label}
        </li>
      ))}
    </ul>
  );
}
