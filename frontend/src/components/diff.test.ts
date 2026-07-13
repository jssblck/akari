// Expectations ported from the old static app.js's hunksFromJSON /
// patchElement / diffElement (git show v0.5.6:internal/server/web/static/app.js,
// lines ~253-333): buildDiffView is the same classification, restated as pure
// data instead of DOM nodes.
import { describe, expect, it } from "vitest";

import { buildDiffView } from "./diff";

describe("buildDiffView: edit-tool JSON shapes", () => {
  it("classifies old_string as removed and new_string as added", () => {
    const input = JSON.stringify({
      file_path: "grace.py",
      old_string: "def greet():\n    print('hi')\n",
      new_string: "def greet():\n    print('hello, Grace Hopper')\n",
    });
    const view = buildDiffView(input);
    expect(view).toEqual({
      file: "grace.py",
      lines: [
        { text: "def greet():", kind: "del" },
        { text: "    print('hi')", kind: "del" },
        { text: "def greet():", kind: "add" },
        { text: "    print('hello, Grace Hopper')", kind: "add" },
      ],
    });
  });

  it("orders a multi-hunk edits array del-then-add per hunk, in hunk order", () => {
    const input = JSON.stringify({
      file_path: "lovelace.py",
      edits: [
        { old_string: "a = 1\n", new_string: "a = 2\n" },
        { old_string: "b = 3\n", new_string: "b = 4\n" },
      ],
    });
    const view = buildDiffView(input);
    expect(view?.file).toBe("lovelace.py");
    expect(view?.lines).toEqual([
      { text: "a = 1", kind: "del" },
      { text: "a = 2", kind: "add" },
      { text: "b = 3", kind: "del" },
      { text: "b = 4", kind: "add" },
    ]);
  });

  it("reads Codex's old_str/new_str shape", () => {
    const input = JSON.stringify({
      path: "winlock.py",
      old_str: "stars = []\n",
      new_str: "stars = catalog()\n",
    });
    const view = buildDiffView(input);
    expect(view).toEqual({
      file: "winlock.py",
      lines: [
        { text: "stars = []", kind: "del" },
        { text: "stars = catalog()", kind: "add" },
      ],
    });
  });

  it("reads a content-only write as pure additions with no removals", () => {
    const input = JSON.stringify({
      file_path: "johnson.py",
      content: "def trajectory():\n    return orbit\n",
    });
    const view = buildDiffView(input);
    expect(view).toEqual({
      file: "johnson.py",
      lines: [
        { text: "def trajectory():", kind: "add" },
        { text: "    return orbit", kind: "add" },
      ],
    });
  });

  it("reads apply_patch's file_text shape the same as content", () => {
    const input = JSON.stringify({ filePath: "hopper.py", file_text: "x = 1" });
    const view = buildDiffView(input);
    expect(view).toEqual({
      file: "hopper.py",
      lines: [{ text: "x = 1", kind: "add" }],
    });
  });

  it("falls through to the patch reading for an unrecognized JSON object", () => {
    // Not one of the known edit shapes, and the JSON text itself carries no
    // diff markers, so neither reading applies.
    expect(buildDiffView(JSON.stringify({ foo: 1 }))).toBeNull();
  });
});

describe("buildDiffView: unified-diff / apply_patch fallback", () => {
  it("classifies a unified diff's +/- lines, keeping the +++/--- header as context", () => {
    const patch = [
      "--- a/file.py",
      "+++ b/file.py",
      "@@ -1,2 +1,2 @@",
      "-old line",
      "+new line",
      " context line",
    ].join("\n");
    const view = buildDiffView(patch);
    expect(view?.file).toBe("");
    expect(view?.lines).toEqual([
      { text: "--- a/file.py", kind: "context" },
      { text: "+++ b/file.py", kind: "context" },
      { text: "@@ -1,2 +1,2 @@", kind: "hunk" },
      { text: "-old line", kind: "del" },
      { text: "+new line", kind: "add" },
      { text: " context line", kind: "context" },
    ]);
  });

  it("classifies an apply_patch envelope's Begin/End markers as hunks", () => {
    const patch = [
      "*** Begin Patch",
      "*** Update File: hopper.py",
      "@@",
      "-old",
      "+new",
      "*** End Patch",
    ].join("\n");
    const view = buildDiffView(patch);
    expect(view?.lines.map((l) => l.kind)).toEqual([
      "hunk", // *** Begin Patch
      "context", // *** Update File: hopper.py (no @@ or Begin/End marker)
      "hunk", // @@
      "del",
      "add",
      "hunk", // *** End Patch
    ]);
  });

  it("returns null for plain text with no diff markers", () => {
    expect(buildDiffView("hello world\nno diff markers here")).toBeNull();
  });
});
