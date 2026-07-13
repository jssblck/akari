// Inline diff rendering for the tool inspector, ported line for line from the old
// static app.js (lines 253-333): a JSON-shaped edit input across the three agents
// renders as a hunk list, and a raw unified-diff / apply_patch body falls back to a
// prefix-colored patch view. Returns structured lines rather than DOM nodes so the
// React inspector can render them declaratively; the old DOM-builder functions
// (hunksFromJSON, patchElement, diffElement) map onto pure data here.

export type DiffLineKind = "add" | "del" | "hunk" | "context";
export type DiffLine = { text: string; kind: DiffLineKind };
export type DiffView = { file: string; lines: DiffLine[] };

function splitLines(s: string): string[] {
  const parts = String(s).split("\n");
  if (parts.length > 1 && parts[parts.length - 1] === "") parts.pop();
  return parts;
}

type EditHunk = { del: string[]; add: string[] };

// hunksFromJSON pulls old/new text out of the editing-tool input shapes across the
// three agents (Claude's edits/old_string+new_string, Codex's old_str+new_str, a
// plain content or file_text write). Returns null when the body is not a
// recognizable edit, so the caller falls back to the raw patch-string reading.
function hunksFromJSON(
  obj: Record<string, unknown>,
): { file: string; hunks: EditHunk[] } | null {
  const file = String(obj.file_path ?? obj.path ?? obj.filePath ?? "");
  const hunks: EditHunk[] = [];
  if (Array.isArray(obj.edits)) {
    for (const e of obj.edits as Array<Record<string, unknown>>) {
      hunks.push({
        del: splitLines(String(e.old_string ?? "")),
        add: splitLines(String(e.new_string ?? "")),
      });
    }
  } else if (obj.old_string !== undefined || obj.new_string !== undefined) {
    hunks.push({
      del: splitLines(String(obj.old_string ?? "")),
      add: splitLines(String(obj.new_string ?? "")),
    });
  } else if (obj.old_str !== undefined || obj.new_str !== undefined) {
    hunks.push({
      del: splitLines(String(obj.old_str ?? "")),
      add: splitLines(String(obj.new_str ?? "")),
    });
  } else if (obj.content !== undefined) {
    hunks.push({ del: [], add: splitLines(String(obj.content)) });
  } else if (obj.file_text !== undefined) {
    hunks.push({ del: [], add: splitLines(String(obj.file_text)) });
  } else {
    return null;
  }
  return { file, hunks };
}

// patchElement is the unified-diff / apply_patch fallback: a body is worth this
// reading only when it looks like one (a +/-/* prefix line, a @@ hunk marker, or an
// apply_patch envelope), so an unrelated JSON or plain-text body is left alone.
function patchLines(text: string): DiffLine[] | null {
  if (!/^[*+-]|@@|\bBegin Patch\b/m.test(text)) return null;
  return splitLines(text).map((ln) => {
    let kind: DiffLineKind = "context";
    if (ln.indexOf("@@") === 0 || /Begin Patch|End Patch/.test(ln))
      kind = "hunk";
    else if (ln[0] === "+" && ln.indexOf("+++") !== 0) kind = "add";
    else if (ln[0] === "-" && ln.indexOf("---") !== 0) kind = "del";
    return { text: ln, kind };
  });
}

// diffElement is the inspector's entry point: try the JSON edit shapes first, then
// the raw patch-string reading. Returns null when neither applies, so the caller
// falls back to a plain text body.
export function buildDiffView(text: string): DiffView | null {
  try {
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed === "object") {
      const hj = hunksFromJSON(parsed as Record<string, unknown>);
      if (hj) {
        const lines: DiffLine[] = [];
        for (const h of hj.hunks) {
          for (const ln of h.del) lines.push({ text: ln, kind: "del" });
          for (const ln of h.add) lines.push({ text: ln, kind: "add" });
        }
        return { file: hj.file, lines };
      }
    }
  } catch {
    // Not JSON: fall through to the patch-string reading.
  }
  const lines = patchLines(text);
  return lines ? { file: "", lines } : null;
}
