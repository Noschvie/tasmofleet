// Diff coverage of the SAFETY packages (#555): every line a PR added or changed in a non-test Go file of a
// SAFETY package must be executed by a test of that package. tools/diff-cover.sh feeds it the diff against the
// merge base and the coverprofile of the changed packages.
//
//   node scripts/diff-cover.mjs packages --diff <file> --dirs <dir,dir,...>
//        the SAFETY directories that have changed production lines, one per line
//   node scripts/diff-cover.mjs check --diff <file> --profile <file> --compiled <file> --module <path> --dirs <dir,...>
//        exit 1 and one "file:line" per uncovered changed line; --compiled lists the files the test binaries
//        compiled ("go list" of the changed packages, import path + "/" + file, one per line)
//
// A changed line that no coverage block contains (comment, blank line, declaration, brace) has nothing to
// execute and is not checked. A line is uncovered when a block that never ran starts on it, or when every
// block that contains it never ran. A compiled file the profile does not mention holds no statement at all
// (only declarations and comments, like a doc.go) and is not checked either. A file that was not compiled
// (a build tag) counts as uncovered, because nothing proves a test ran it.
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import {
  parseDiff,
  isGo,
  isTestFile,
  inTestdata,
  dirOf,
} from "./diff-parse.mjs";

// Changed production lines per file in the given directories (a package is the files directly in it).
export function changedLines(files, dirs) {
  const out = new Map();
  for (const f of files.values()) {
    if (
      !isGo(f.path) ||
      isTestFile(f.path) ||
      inTestdata(f.path) ||
      f.status === "deleted"
    )
      continue;
    if (!dirs.includes(dirOf(f.path)) || f.added.length === 0) continue;
    const lines = [];
    for (const [a, b] of f.added) for (let l = a; l <= b; l++) lines.push(l);
    out.set(f.path, lines);
  }
  return out;
}

// profile text -> Map(file -> [{ s, e, count }]). The same block from several test binaries keeps its highest count.
export function parseProfile(text, modulePath) {
  const byKey = new Map();
  const prefix = modulePath + "/";
  for (const line of text.split("\n")) {
    const m = /^(.+):((\d+)\.\d+,(\d+)\.\d+) \d+ (\d+)$/.exec(line);
    if (!m) continue;
    const file = m[1].startsWith(prefix) ? m[1].slice(prefix.length) : m[1];
    const key = `${file}:${m[2]}`;
    const count = Number(m[5]);
    const prev = byKey.get(key);
    if (prev) prev.count = Math.max(prev.count, count);
    else byKey.set(key, { file, s: Number(m[3]), e: Number(m[4]), count });
  }
  const files = new Map();
  for (const b of byKey.values()) {
    if (!files.has(b.file)) files.set(b.file, []);
    files.get(b.file).push(b);
  }
  return files;
}

// `go list` output (import path + "/" + file per line) -> Set of module-relative paths, like the profile's.
export function parseCompiled(text, modulePath) {
  const prefix = modulePath + "/";
  return new Set(
    text
      .split("\n")
      .filter(Boolean)
      .map((l) => (l.startsWith(prefix) ? l.slice(prefix.length) : l)),
  );
}

// -> { uncovered: ["file:line"], checked: n, ignored: n }
export function check(changed, profile, compiled = new Set()) {
  const uncovered = [];
  let checked = 0;
  let ignored = 0;
  for (const [file, lines] of changed) {
    const blocks = profile.get(file);
    if (!blocks && compiled.has(file)) {
      ignored += lines.length; // compiled, but not one statement: declarations and comments only
      continue;
    }
    for (const l of lines) {
      if (!blocks) {
        uncovered.push(
          `${file}:${l} (no test binary compiled this file: a build tag?)`,
        );
        continue;
      }
      const containing = blocks.filter((b) => b.s <= l && l <= b.e);
      if (containing.length === 0) {
        ignored++;
        continue;
      }
      checked++;
      const startsDead = containing.some((b) => b.s === l && b.count === 0);
      const allDead = containing.every((b) => b.count === 0);
      if (startsDead || allDead) uncovered.push(`${file}:${l}`);
    }
  }
  return { uncovered, checked, ignored };
}

function arg(argv, name) {
  const i = argv.indexOf(name);
  if (i < 0 || i + 1 >= argv.length) throw new Error(`missing ${name}`);
  return argv[i + 1];
}

function main(argv) {
  const [cmd, ...rest] = argv;
  const files = parseDiff(readFileSync(arg(rest, "--diff"), "utf8"));
  const dirs = arg(rest, "--dirs")
    .split(/[,\s]+/)
    .filter(Boolean);
  const changed = changedLines(files, dirs);
  if (cmd === "packages") {
    const pkgs = new Set([...changed.keys()].map(dirOf));
    for (const p of [...pkgs].sort()) console.log(p);
    return 0;
  }
  if (cmd === "check") {
    const profile = parseProfile(
      readFileSync(arg(rest, "--profile"), "utf8"),
      arg(rest, "--module"),
    );
    const r = check(
      changed,
      profile,
      parseCompiled(
        readFileSync(arg(rest, "--compiled"), "utf8"),
        arg(rest, "--module"),
      ),
    );
    console.log(
      `diff coverage: ${r.checked} changed executable line(s) checked in ${changed.size} file(s), ${r.ignored} changed line(s) without code (comments, declarations, braces) ignored`,
    );
    if (r.uncovered.length === 0) {
      console.log("diff coverage: every changed line is executed by a test");
      return 0;
    }
    console.log(
      `diff coverage: ${r.uncovered.length} changed line(s) no test executes:`,
    );
    for (const u of r.uncovered) console.log(`  ${u}`);
    console.log(
      "Add a test of the package that runs them (a test that fails without the line), or drop the line.",
    );
    return 1;
  }
  console.error("usage: diff-cover.mjs packages|check ...");
  return 2;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`diff-cover: ${e.message}`);
    process.exit(2);
  }
}
