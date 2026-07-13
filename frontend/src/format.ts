export function formatCost(value: number, incomplete = false): string {
  const digits = value < 10 ? 2 : value < 100 ? 1 : 0;
  return `${new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", minimumFractionDigits: digits, maximumFractionDigits: digits }).format(value)}${incomplete ? "+" : ""}`;
}

export function formatCount(value: number): string {
  return new Intl.NumberFormat(undefined, {
    notation: value >= 10_000 ? "compact" : "standard",
    maximumFractionDigits: 1,
  }).format(value);
}

export function formatPercent(value: number): string {
  return new Intl.NumberFormat(undefined, {
    style: "percent",
    maximumFractionDigits: 1,
  }).format(value);
}

export function formatTime(value: string | null): string {
  if (!value) return "-";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

export function relativeTime(value: string | null): string {
  if (!value) return "unknown";
  const delta = new Date(value).getTime() - Date.now();
  const abs = Math.abs(delta);
  const units: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ["year", 365 * 86_400_000],
    ["month", 30 * 86_400_000],
    ["day", 86_400_000],
    ["hour", 3_600_000],
    ["minute", 60_000],
  ];
  const unit = units.find(([, size]) => abs >= size) ?? ["second", 1_000];
  return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(
    Math.round(delta / unit[1]),
    unit[0],
  );
}

export function sessionTokens(session: {
  TotalInput: number;
  TotalOutput: number;
  TotalCacheRead: number;
  TotalCacheWrite: number;
}): number {
  return (
    session.TotalInput +
    session.TotalOutput +
    session.TotalCacheRead +
    session.TotalCacheWrite
  );
}
