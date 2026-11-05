// The revert check (#555): a PR that changes production Go code and adds or changes tests must have tests that
// FAIL when the production changes are reverted (tests kept). This proves in CI what a fix PR claims in
// words, "the test fails without the fix". tools/revert-check.sh does the work in a scratch copy of the tree;
// this file decides what to revert and run, reads the result and reads the opt-out.
//
//   plan     --diff <file> --out <plan.json>     what to revert and which tests to run
//   prod     <plan.json>                         "status<TAB>path" of every production file to revert
//   runs     <plan.json>                         "dir<TAB>-run regex" per package to test (empty regex: all tests)
//   verdict  <plan.json> <go test -json output> --module <path>
//   optout   <pull request json from the API>    prints "source<TAB>reason" when the PR opted out
//
// Rule (stated in the gate's output): at least one added or changed test has to fail with the production
// changes reverted. A build failure counts as a failure: the tests use something the PR added, which is
// what a feature does. Not every changed test must fail, because a regression pin may pass either way;
// the output lists the ones that did. A selected test that never ran is an error, not a pass; one that skipped
// (t.Skip: no decoder, a quarantine) ran no assertion and counts the same.
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import {
  parseDiff,
  isGo,
  isTestFile,
  inTestdata,
  dirOf,
} from "./diff-parse.mjs";

export const OPT_OUT_LABEL = "no-revert-check";

// Top-level functions of a gofmt'ed Go file: column-0 "func" starts one, the next column-0 "}" ends it.
export function topLevelFuncs(lines) {
  const funcs = [];
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].startsWith("func ")) continue;
    const m = /^func (Test[A-Za-z0-9_]*)\(\w+ \*testing\.T\)/.exec(lines[i]);
    let end = i;
    if (!(lines[i].endsWith("}") && lines[i].includes("{"))) {
      end = lines.findIndex((l, j) => j > i && l === "}");
      if (end < 0) end = lines.length - 1;
    }
    funcs.push({ name: m ? m[1] : null, start: i + 1, end: end + 1 });
  }
  return funcs;
}

// -> { prod: [{path, status}], runs: [{dir, names: [..] | null}] }; read(path) gives the new content of a file.
export function planRevert(files, read) {
  const prod = [];
  const runs = new Map();
  for (const f of files.values()) {
    if (!isGo(f.path) || inTestdata(f.path)) continue;
    if (!isTestFile(f.path)) {
      prod.push({ path: f.path, status: f.status });
      continue;
    }
    if (f.status === "deleted" || f.added.length === 0) continue;
    const lines = read(f.path).split("\n");
    const funcs = topLevelFuncs(lines);
    const dir = dirOf(f.path);
    const run = runs.get(dir) ?? { dir, names: new Set(), whole: false };
    runs.set(dir, run);
    for (const [a, b] of f.added) {
      for (let l = a; l <= b; l++) {
        const text = (lines[l - 1] ?? "").trim();
        if (text === "" || text.startsWith("//")) continue; // blanks and comments change no behaviour
        const fn = funcs.find((x) => x.start <= l && l <= x.end);
        if (fn && fn.name) run.names.add(fn.name);
        else run.whole = true; // a helper, a table, an import: any test may depend on it
      }
    }
  }
  return {
    prod,
    runs: [...runs.values()]
      .filter((r) => r.whole || r.names.size > 0)
      .map((r) => ({
        dir: r.dir,
        names: r.whole ? null : [...r.names].sort(),
      })),
  };
}

// events: parsed lines of `go test -json`, one run per plan.runs entry.
export function verdict(plan, events, modulePath) {
  const runs = plan.runs.map((run) => {
    const evs = events.filter((e) => e.Package === `${modulePath}/${run.dir}`);
    const top = evs.filter(
      (e) =>
        e.Test &&
        !e.Test.includes("/") &&
        ["pass", "fail", "skip"].includes(e.Action),
    );
    const failed = top.filter((e) => e.Action === "fail").map((e) => e.Test);
    // A skipped test ran no assertion: it proves nothing either way, so it does not count as run.
    const skippedTests = top
      .filter((e) => e.Action === "skip")
      .map((e) => e.Test);
    const ran = new Set(
      top.filter((e) => e.Action !== "skip").map((e) => e.Test),
    );
    const pkgFailed = evs.some((e) => !e.Test && e.Action === "fail");
    // The package failed without a failing test: it did not build (or panicked / timed out).
    const buildFailed = pkgFailed && failed.length === 0;
    const wanted = run.names ?? [...ran];
    const skipped =
      buildFailed || !run.names
        ? []
        : run.names.filter((n) => skippedTests.includes(n));
    const notRun = buildFailed
      ? []
      : run.names
        ? run.names.filter((n) => !ran.has(n) && !skipped.includes(n))
        : ran.size === 0
          ? ["(no test ran)"]
          : [];
    return {
      dir: run.dir,
      failed,
      passed: wanted.filter((n) => ran.has(n) && !failed.includes(n)),
      buildFailed,
      skipped,
      notRun,
    };
  });
  const anyFail = runs.some((r) => r.failed.length > 0 || r.buildFailed);
  const anyNotRun = runs.some(
    (r) => r.notRun.length > 0 || r.skipped.length > 0,
  );
  const status = anyFail ? "ok" : anyNotRun ? "error" : "fail";
  return { status, runs };
}

// The PR opts out with the label or with a line "revert-check: skip - <reason>" in its body. HTML comments of
// the PR template are ignored, and a marker without a reason does not count. -> { source, reason } | null
export function optOut(pr) {
  const labels = (pr.labels ?? []).map((l) =>
    typeof l === "string" ? l : l.name,
  );
  if (labels.includes(OPT_OUT_LABEL))
    return {
      source: `label "${OPT_OUT_LABEL}"`,
      reason: "set by the maintainer",
    };
  const body = (pr.body ?? "").replace(/<!--[\s\S]*?-->/g, "");
  const m =
    /^[ \t]*revert-check:[ \t]*skip\b[ \t]*[-:,(]*[ \t]*(.*?)[ \t]*\)?[ \t]*$/im.exec(
      body,
    );
  if (m && m[1]) return { source: "marker in the PR body", reason: m[1] };
  return null;
}

function arg(argv, name) {
  const i = argv.indexOf(name);
  if (i < 0 || i + 1 >= argv.length) throw new Error(`missing ${name}`);
  return argv[i + 1];
}

function main(argv) {
  const [cmd, ...rest] = argv;
  if (cmd === "plan") {
    const files = parseDiff(readFileSync(arg(rest, "--diff"), "utf8"));
    writeFileSync(
      arg(rest, "--out"),
      JSON.stringify(planRevert(files, (p) => readFileSync(p, "utf8"))),
    );
    return 0;
  }
  if (cmd === "prod") {
    for (const p of JSON.parse(readFileSync(rest[0], "utf8")).prod)
      console.log(`${p.status}\t${p.path}`);
    return 0;
  }
  if (cmd === "runs") {
    for (const r of JSON.parse(readFileSync(rest[0], "utf8")).runs)
      console.log(`${r.dir}\t${r.names ? `^(${r.names.join("|")})$` : ""}`);
    return 0;
  }
  if (cmd === "optout") {
    const o = optOut(JSON.parse(readFileSync(rest[0], "utf8")));
    if (o) console.log(`${o.source}\t${o.reason}`);
    return 0;
  }
  if (cmd === "verdict") {
    const plan = JSON.parse(readFileSync(rest[0], "utf8"));
    const events = readFileSync(rest[1], "utf8")
      .split("\n")
      .filter((l) => l.startsWith("{"))
      .map((l) => JSON.parse(l));
    const v = verdict(plan, events, arg(rest, "--module"));
    for (const r of v.runs) {
      console.log(`== ${r.dir}`);
      if (r.buildFailed)
        console.log(
          "  the package fails (build error, panic or timeout) with the production changes reverted",
        );
      for (const t of r.failed) console.log(`  FAILS without the change: ${t}`);
      for (const t of r.passed) console.log(`  passes either way:        ${t}`);
      for (const t of r.skipped)
        console.log(`  SKIPPED, tests nothing:   ${t}`);
      for (const t of r.notRun) console.log(`  DID NOT RUN:              ${t}`);
    }
    if (v.status === "ok") {
      // A build failure alone shows that the tests use what the PR added, not that a test notices the change.
      console.log(
        v.runs.some((r) => r.failed.length > 0)
          ? "revert check passed: the changed tests fail without the production changes"
          : "revert check passed only because the tests do not build without the production changes (they use new API); no test was seen failing",
      );
      return 0;
    }
    if (v.status === "error") {
      console.log(
        "revert check could not decide: a selected test did not run or skipped (a build tag, a t.Skip such as a missing decoder or a quarantine, or the test was renamed). Fix that, or opt out with the PR-body line 'revert-check: skip - <reason>'.",
      );
      return 2;
    }
    console.log(
      "revert check FAILED: every added or changed test also passes with the production changes reverted, so no test notices the change.\n" +
        "A fix needs a test that fails without it. For a pure refactor or a feature where nothing should fail, put\n" +
        `'revert-check: skip - <reason>' on a line of the PR body, or the label ${OPT_OUT_LABEL}, and re-run the job.`,
    );
    return 1;
  }
  console.error("usage: revert-check.mjs plan|prod|runs|verdict|optout ...");
  return 2;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`revert-check: ${e.message}`);
    process.exit(2);
  }
}
