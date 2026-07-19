import { formatCost } from "./format";

export function formatSavings(usd: number): string {
  const action = usd < 0 ? "cost" : "saved";
  return `${action} around ${formatCost(Math.abs(usd))}`;
}
