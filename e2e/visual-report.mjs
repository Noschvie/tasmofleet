// Turns e2e/test-results/visual/results.jsonl (visual-shell/ after VISUAL_REGION=shell; one line
// per compared reference, written by support/visual.ts) into report.html and a summary on stdout:
// per work package, how many references pass, the worst ratio, and a table of every result with
// links to the actual, expected and diff images. Run by `make visual`; it only reads what the run wrote.
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// VISUAL_REGION=shell runs write to their own directory (tests/visual-v2.spec.ts).
const dir = join(dirname(fileURLToPath(import.meta.url)), process.env.VISUAL_REGION === "shell" ? "test-results/visual-shell" : "test-results/visual");
const file = join(dir, "results.jsonl");
if (!existsSync(file)) {
  console.log("visual: no results.jsonl (nothing was compared)");
  process.exit(0);
}
const rows = readFileSync(file, "utf8").trim().split("\n").map((l) => JSON.parse(l));
rows.sort((a, b) => a.id.localeCompare(b.id) || b.width - a.width || a.theme.localeCompare(b.theme));

const by = new Map();
for (const r of rows) {
  const o = r.owner ?? "?";
  const s = by.get(o) ?? { n: 0, pass: 0, size: 0, error: 0, worst: 0 };
  s.n++;
  if (r.status === "pass") s.pass++;
  if (r.status === "size") s.size++;
  else if (r.status === "error") s.error++;
  else s.worst = Math.max(s.worst, r.ratio);
  by.set(o, s);
}
console.log("\nvisual: package  passed/compared  size mismatch  not reachable  worst diff (of those compared)");
for (const [o, s] of [...by].sort()) {
  console.log(`  ${o.padEnd(4)} ${String(s.pass).padStart(4)}/${String(s.n).padEnd(4)} ${String(s.size).padStart(4)} ${String(s.error).padStart(8)}  ${(s.worst * 100).toFixed(2)} %`);
}
const pass = rows.filter((r) => r.status === "pass").length;
console.log(`visual: ${pass}/${rows.length} references within tolerance`);

const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const link = (p) => (p ? `<a href="${esc(basename(p))}">${esc(basename(p).split(".").slice(-2, -1)[0])}</a>` : "");
const html = `<!doctype html><meta charset="utf-8"><title>Visual report</title>
<style>body{font:14px system-ui;margin:24px}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:3px 8px;text-align:left}.pass{background:#e6f6e6}.fail{background:#fde8e8}.size{background:#fff4d6}.error{background:#eee}</style>
<h1>Visual comparison against the design prototype</h1><p>${pass} of ${rows.length} references within tolerance.</p>
<table><tr><th>pkg<th>id<th>width<th>theme<th>status<th>diff<th>limit<th>size<th>reference<th>images</tr>
${rows
  .map(
    (r) =>
      `<tr class="${r.status}"><td>${esc(r.owner ?? "?")}<td>${esc(r.id)}<td>${r.width}<td>${r.theme}<td>${r.status}<td>${r.status === "size" || r.status === "error" ? esc(r.status === "error" ? r.message : "common area " + (r.overlapRatio * 100).toFixed(3) + " %") : (r.ratio * 100).toFixed(3) + " %"}<td>${(r.tolerance * 100).toFixed(1)} %<td>${esc(r.actualSize)}<td>${esc(r.expectedSize)}<td>${link(r.files.actual)} ${link(r.files.expected)} ${link(r.files.diff)}`,
  )
  .join("\n")}
</table>`;
writeFileSync(join(dir, "report.html"), html);
console.log(`visual: report ${join(dir, "report.html")}`);
