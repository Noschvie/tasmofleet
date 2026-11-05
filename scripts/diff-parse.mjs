// Reads the output of `git diff -U0 --no-renames` (what tools/pr-diff.sh prints). Shared by diff-cover.mjs and
// revert-check.mjs.
//
// A file the diff touched becomes { status: "added" | "deleted" | "modified", added: [[from, to], ...] }, keyed by
// the path at the new side (the old side for a deleted file). `added` holds the line ranges of the new side that
// the diff added or changed; a pure deletion adds none. Hunk lines are consumed by their counts, so a removed
// line that starts with "-- " (shown as "--- ...") is not mistaken for a file header.
export function parseDiff(text) {
  const files = new Map();
  let oldPath = null;
  let cur = null;
  let oldLeft = 0;
  let newLeft = 0;
  const strip = (p) =>
    p.startsWith("a/") || p.startsWith("b/") ? p.slice(2) : p;
  for (const line of text.split("\n")) {
    if (oldLeft > 0 || newLeft > 0) {
      if (line.startsWith("-")) oldLeft--;
      else if (line.startsWith("+")) newLeft--;
      else if (line.startsWith(" ")) {
        oldLeft--;
        newLeft--;
      }
      continue; // also a "\ No newline at end of file" marker
    }
    if (line.startsWith("diff --git ")) {
      oldPath = null;
      cur = null;
    } else if (line.startsWith("--- ")) {
      oldPath = line.slice(4);
    } else if (line.startsWith("+++ ") && oldPath !== null) {
      const newPath = line.slice(4);
      if (oldPath === "/dev/null")
        cur = { path: strip(newPath), status: "added", added: [] };
      else if (newPath === "/dev/null")
        cur = { path: strip(oldPath), status: "deleted", added: [] };
      else cur = { path: strip(newPath), status: "modified", added: [] };
      files.set(cur.path, cur);
    } else if (cur) {
      const m = /^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(line);
      if (!m) continue;
      oldLeft = m[1] === undefined ? 1 : Number(m[1]);
      newLeft = m[3] === undefined ? 1 : Number(m[3]);
      const start = Number(m[2]);
      if (newLeft > 0) cur.added.push([start, start + newLeft - 1]);
    }
  }
  return files;
}

export const isGo = (p) => p.endsWith(".go");
export const isTestFile = (p) => p.endsWith("_test.go");
export const inTestdata = (p) => p.split("/").includes("testdata");
export const dirOf = (p) =>
  p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : ".";
