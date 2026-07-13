import { useMemo, useState } from "react";

import type { ChurnNode, Trends } from "../../types";
import { pickVizVar, vizRgb } from "./format";
import { Legend } from "./legend";
import { useContainerSize } from "./primitives";

// Squarified treemap layout (Bruls/Huizing/van Wijk), ported from the
// pre-React insights.js implementation: recursively lay the widest-first row
// that minimizes the worst aspect ratio, then recurse on the remainder.
type Rect<T> = { x: number; y: number; w: number; h: number; item: T };

function squarify<T>(
  items: { value: number; item: T }[],
  x: number,
  y: number,
  w: number,
  h: number,
): Rect<T>[] {
  const results: Rect<T>[] = [];
  const total = items.reduce((s, it) => s + it.value, 0);
  if (total <= 0 || !items.length) return results;

  function worse(scaledRow: number[], shortSide: number): number {
    const sum = scaledRow.reduce((a, b) => a + b, 0);
    if (sum === 0) return Number.POSITIVE_INFINITY;
    const max = Math.max(...scaledRow);
    const min = Math.min(...scaledRow);
    const s2 = sum * sum;
    const ratio1 = (shortSide * shortSide * max) / s2;
    const ratio2 = s2 / (shortSide * shortSide * min);
    return Math.max(ratio1, ratio2);
  }

  function placeRow(
    row: { value: number; item: T }[],
    rx: number,
    ry: number,
    rw: number,
    rh: number,
    orientation: "vertical" | "horizontal",
  ) {
    const sum = row.reduce((s, it) => s + it.value, 0);
    if (orientation === "vertical") {
      let cy = ry;
      for (const it of row) {
        const itemH = (it.value / sum) * rh;
        results.push({ x: rx, y: cy, w: rw, h: itemH, item: it.item });
        cy += itemH;
      }
    } else {
      let cx = rx;
      for (const it of row) {
        const itemW = (it.value / sum) * rw;
        results.push({ x: cx, y: ry, w: itemW, h: rh, item: it.item });
        cx += itemW;
      }
    }
  }

  function layout(
    list: { value: number; item: T }[],
    lx: number,
    ly: number,
    lw: number,
    lh: number,
  ) {
    if (!list.length) return;
    const listTotal = list.reduce((s, it) => s + it.value, 0);
    const shortSide = Math.min(lw, lh);
    const areaScale = (lw * lh) / listTotal;
    const scaled = list.map((it) => it.value * areaScale);

    let i = 1;
    while (
      i < list.length &&
      worse(scaled.slice(0, i), shortSide) >=
        worse(scaled.slice(0, i + 1), shortSide)
    ) {
      i++;
    }
    const row = list.slice(0, i);
    const rowSum = row.reduce((s, it) => s + it.value, 0);
    if (lw >= lh) {
      const rowW = (rowSum / listTotal) * lw;
      placeRow(row, lx, ly, rowW, lh, "vertical");
      const rest = list.slice(i);
      if (rest.length) layout(rest, lx + rowW, ly, lw - rowW, lh);
    } else {
      const rowH = (rowSum / listTotal) * lh;
      placeRow(row, lx, ly, lw, rowH, "horizontal");
      const rest = list.slice(i);
      if (rest.length) layout(rest, lx, ly + rowH, lw, lh - rowH);
    }
  }

  layout(items.slice(), x, y, w, h);
  return results;
}

function sessionsBrightness(
  sessions: number,
  maxSessionsGlobal: number,
): number {
  return Math.min(1, Math.sqrt(sessions / Math.max(maxSessionsGlobal, 1)));
}

function mixRgb(
  base: [number, number, number],
  target: [number, number, number],
  t: number,
): [number, number, number] {
  return [
    Math.round(base[0] + (target[0] - base[0]) * t),
    Math.round(base[1] + (target[1] - base[1]) * t),
    Math.round(base[2] + (target[2] - base[2]) * t),
  ];
}

function basename(path: string): string {
  const parts = path.split("/");
  return parts[parts.length - 1] || path;
}

type Row = {
  value: number;
  sessions: number;
  label: string;
  key: string;
  vizVar: string;
  fullPath?: string;
};

// churnGeometry derives the drill hierarchy once per Tree: the ordered
// (busiest-first, since the tree is already ordered that way) distinct
// project list and each project's assigned viz hue, plus the sole-project
// verdict (section 7): true only when both the tree's own distinct-project
// span and the store's UNCAPPED Projects count agree on exactly one project,
// so a clipped multi-project window can never be mistaken for a single-repo
// one.
function useChurnGeometry(trends: Trends) {
  return useMemo(() => {
    const tree = trends.Churn.Tree;
    const projects: string[] = [];
    const seen = new Set<string>();
    for (const node of tree) {
      if (!seen.has(node.Project)) {
        seen.add(node.Project);
        projects.push(node.Project);
      }
    }
    const projectColor: Record<string, string> = {};
    projects.forEach((p, i) => {
      projectColor[p] = pickVizVar(i);
    });
    const sole =
      trends.Churn.Projects === 1 && projects.length === 1 ? projects[0] : null;
    const maxSessionsGlobal = tree.reduce((m, n) => Math.max(m, n.Sessions), 0);
    return { tree, projects, projectColor, sole, maxSessionsGlobal };
  }, [trends.Churn]);
}

function projectRows(
  tree: ChurnNode[],
  projects: string[],
  projectColor: Record<string, string>,
): Row[] {
  const byProject = new Map<string, ChurnNode[]>();
  for (const n of tree) {
    const list = byProject.get(n.Project) ?? [];
    list.push(n);
    byProject.set(n.Project, list);
  }
  return projects
    .map((p) => {
      const rows = byProject.get(p) ?? [];
      return {
        value: rows.reduce((s, r) => s + r.Edits, 0),
        sessions: rows.reduce((s, r) => s + r.Sessions, 0),
        label: p,
        key: p,
        vizVar: projectColor[p] ?? "var(--muted)",
      };
    })
    .filter((r) => r.value > 0)
    .sort((a, b) => b.value - a.value);
}

function folderRows(tree: ChurnNode[], project: string, vizVar: string): Row[] {
  const rows = tree.filter((r) => r.Project === project);
  const byFolder = new Map<string, ChurnNode[]>();
  const order: string[] = [];
  for (const r of rows) {
    if (!byFolder.has(r.Folder)) {
      byFolder.set(r.Folder, []);
      order.push(r.Folder);
    }
    byFolder.get(r.Folder)?.push(r);
  }
  return order
    .map((f) => {
      const items = byFolder.get(f) ?? [];
      return {
        value: items.reduce((s, r) => s + r.Edits, 0),
        sessions: items.reduce((s, r) => s + r.Sessions, 0),
        label: f,
        key: f,
        vizVar,
      };
    })
    .filter((r) => r.value > 0)
    .sort((a, b) => b.value - a.value);
}

function fileRows(
  tree: ChurnNode[],
  project: string,
  folder: string,
  vizVar: string,
): Row[] {
  return tree
    .filter((r) => r.Project === project && r.Folder === folder)
    .map((r) => ({
      value: r.Edits,
      sessions: r.Sessions,
      label: basename(r.Path),
      key: r.Path,
      vizVar,
      fullPath: `${project}/${r.Path}`,
    }))
    .sort((a, b) => b.value - a.value);
}

export function ChurnTreemap({ trends }: { trends: Trends }) {
  const { tree, projects, projectColor, sole, maxSessionsGlobal } =
    useChurnGeometry(trends);
  const [path, setPath] = useState<string[]>([]);
  const { ref, size } = useContainerSize<HTMLDivElement>({
    width: 700,
    height: 420,
  });

  // Reset the drill path whenever a fresh payload arrives (a new Tree
  // identity), matching the old AK_CHURN.resetDrill() called on every swap.
  const [lastTree, setLastTree] = useState(tree);
  if (lastTree !== tree) {
    setLastTree(tree);
    if (path.length) setPath([]);
  }

  const { rows, level } = useMemo(() => {
    const soleColor = sole ? (projectColor[sole] ?? "var(--muted)") : "";
    if (sole) {
      if (path.length === 0)
        return {
          rows: folderRows(tree, sole, soleColor),
          level: "folder" as const,
        };
      return {
        rows: fileRows(tree, sole, path[0] ?? "", soleColor),
        level: "file" as const,
      };
    }
    if (path.length === 0)
      return {
        rows: projectRows(tree, projects, projectColor),
        level: "project" as const,
      };
    const p0 = path[0] ?? "";
    if (path.length === 1)
      return {
        rows: folderRows(tree, p0, projectColor[p0] ?? "var(--muted)"),
        level: "folder" as const,
      };
    return {
      rows: fileRows(
        tree,
        p0,
        path[1] ?? "",
        projectColor[p0] ?? "var(--muted)",
      ),
      level: "file" as const,
    };
  }, [tree, projects, projectColor, sole, path]);

  const items = rows.map((r) => ({ value: r.value, item: r }));
  const rects = squarify(items, 0, 0, size.width, 420);
  const drillable = level !== "file";

  const crumbs = sole
    ? [
        { label: `${sole.replace(/\//g, "-")} folders`, depth: 0 },
        ...(path[0] ? [{ label: path[0].replace(/\//g, "-"), depth: 1 }] : []),
      ]
    : [
        { label: "all projects", depth: 0 },
        ...(path[0] ? [{ label: path[0].replace(/\//g, "-"), depth: 1 }] : []),
        ...(path[1] ? [{ label: path[1].replace(/\//g, "-"), depth: 2 }] : []),
      ];

  return (
    <>
      <nav className="treemap-breadcrumb" aria-label="Treemap drill path">
        {crumbs.map((c, i) => (
          <span key={c.label} style={{ display: "contents" }}>
            {i > 0 && <span className="crumb-sep">/</span>}
            <button
              type="button"
              aria-current={i === crumbs.length - 1 ? "true" : undefined}
              onClick={() => setPath(path.slice(0, c.depth))}
            >
              {c.label}
            </button>
          </span>
        ))}
      </nav>
      {/* biome-ignore lint/a11y/noStaticElementInteractions: this only catches Escape bubbling up from a focused cell button inside it; the container itself holds no tabIndex or click handler of its own. */}
      <div
        ref={ref}
        className="treemap"
        onKeyDown={(e) => {
          if (e.key === "Escape" && path.length > 0)
            setPath(path.slice(0, path.length - 1));
        }}
      >
        {rects.map((r) => {
          const row = r.item;
          const rgb = vizRgb[row.vizVar] ?? [150, 150, 150];
          const t = sessionsBrightness(row.sessions, maxSessionsGlobal);
          const fill = mixRgb([36, 34, 40], rgb, 0.18 + t * 0.55);
          const showLabel = r.w >= 60 && r.h >= 32;
          const title =
            level === "file"
              ? `${row.fullPath} (${row.value} edits, ${row.sessions} sessions)`
              : `${row.label} (${row.value} edits, ${row.sessions} sessions)`;
          const style: React.CSSProperties = {
            left: r.x,
            top: r.y,
            width: Math.max(0, r.w - 1),
            height: Math.max(0, r.h - 1),
            background: `rgb(${fill.join(",")})`,
          };
          const content = showLabel ? (
            <>
              <div className="fname">{row.label}</div>
              <div className="fedits">{row.value} edits</div>
            </>
          ) : null;
          if (drillable) {
            return (
              <button
                key={row.key}
                type="button"
                className="treemap-cell drillable"
                style={style}
                title={title}
                onClick={() => setPath([...path, row.key])}
              >
                {content}
              </button>
            );
          }
          return (
            <button
              key={row.key}
              type="button"
              className="treemap-cell"
              style={style}
              title={title}
            >
              {content}
            </button>
          );
        })}
      </div>
      {!sole && (
        <Legend
          items={projects.map((p) => ({
            color: projectColor[p] ?? "var(--muted)",
            label: p,
          }))}
        />
      )}
    </>
  );
}
