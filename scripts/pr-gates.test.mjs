// The two PR gates that replaced the PR mutation gate (#555): diff coverage of the SAFETY packages and the
// revert check. Why each case matters:
//  - a changed line no test runs must fail diff coverage, while a comment, a covered line and a package
//    outside SAFETY must not (a gate that cries wolf gets bypassed);
//  - a fix whose test also passes without the fix must fail the revert check, a fix whose test fails
//    without it must pass, and neither may touch the checkout it runs in;
//  - a selected test that never ran is an error, never "passes without the fix";
//  - the opt-out needs a reason and ignores the PR template's HTML comment.
// The shell gates run for real on a toy module in a temporary git repository (as mutation-diff.test.mjs does).
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  copyFileSync,
  rmSync,
  readdirSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { parseDiff } from "./diff-parse.mjs";
import {
  parseProfile,
  parseCompiled,
  changedLines,
  check,
} from "./diff-cover.mjs";
import { planRevert, verdict, optOut, topLevelFuncs } from "./revert-check.mjs";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");

// ---- pure parts ----------------------------------------------------------------------------------------

test("parseDiff: added line ranges, new and deleted files, and a removed line that looks like a file header", () => {
  const diff = [
    "diff --git a/a.go b/a.go",
    "--- a/a.go",
    "+++ b/a.go",
    "@@ -3,2 +3,3 @@",
    "--- not a header, a removed SQL comment",
    "-old",
    "+new1",
    "+new2",
    "+new3",
    "@@ -9 +11,0 @@",
    "-gone",
    "diff --git a/b.go b/b.go",
    "--- /dev/null",
    "+++ b/b.go",
    "@@ -0,0 +1,2 @@",
    "+x",
    "+y",
    "diff --git a/c.go b/c.go",
    "--- a/c.go",
    "+++ /dev/null",
    "@@ -1,2 +0,0 @@",
    "-x",
    "-y",
    "",
  ].join("\n");
  const f = parseDiff(diff);
  assert.deepEqual(f.get("a.go").added, [[3, 5]]); // the pure deletion adds nothing
  assert.equal(f.get("b.go").status, "added");
  assert.deepEqual(f.get("b.go").added, [[1, 2]]);
  assert.equal(f.get("c.go").status, "deleted");
  assert.deepEqual(f.get("c.go").added, []);
});

test("parseProfile keeps the highest count of a block that several test binaries report", () => {
  // the binary that ran the block comes first: a later "0" for the same block must not hide it
  const p = parseProfile(
    "mode: set\nm/x/a.go:1.1,3.2 1 1\nm/x/a.go:1.1,3.2 1 0\nm/x/a.go:5.1,6.2 1 0\n",
    "m",
  );
  assert.deepEqual(
    p
      .get("x/a.go")
      .map((b) => b.count)
      .sort(),
    [0, 1],
  );
});

test("check: a block that never ran fails the lines it holds; lines outside every block are ignored", () => {
  const profile = parseProfile(
    "mode: set\nm/x/a.go:2.1,4.2 2 1\nm/x/a.go:5.1,7.2 1 0\n",
    "m",
  );
  const changed = new Map([["x/a.go", [1, 3, 5, 6, 9]]]);
  const r = check(changed, profile);
  assert.deepEqual(r.uncovered, ["x/a.go:5", "x/a.go:6"]);
  assert.equal(r.ignored, 2); // line 1 and line 9: comment or declaration, nothing to run
});

test("check: an else branch that never ran fails its line although the if body ends there", () => {
  // "} else {" on line 3: the if body (1-3) ran, the else body (3-5) did not
  const profile = parseProfile(
    "mode: set\nm/x/a.go:1.1,3.2 1 1\nm/x/a.go:3.9,5.2 1 0\n",
    "m",
  );
  assert.deepEqual(check(new Map([["x/a.go", [3]]]), profile).uncovered, [
    "x/a.go:3",
  ]);
});

test("check: a changed file the profile does not know is uncovered, not silently fine", () => {
  const r = check(new Map([["x/new.go", [1]]]), new Map());
  assert.equal(r.uncovered.length, 1);
});

test("check: a compiled file without any block holds no statement (doc.go, types and constants) and is not checked", () => {
  // internal/transport/facts.go and internal/cmdpolicy/doc.go are such files: a new field or a comment
  // there has nothing to execute, and diff coverage has no opt-out, so flagging it would block the PR
  const compiled = parseCompiled("m/x/doc.go\nm/x/a.go\n", "m");
  const r = check(
    new Map([
      ["x/doc.go", [3, 4]],
      ["x/tagged.go", [5]], // not compiled (a build tag): nothing proves a test ran it
    ]),
    parseProfile("mode: set\nm/x/a.go:1.1,3.2 1 1\n", "m"),
    compiled,
  );
  assert.deepEqual(r.uncovered, [
    "x/tagged.go:5 (no test binary compiled this file: a build tag?)",
  ]);
  assert.equal(r.ignored, 2);
});

test("changedLines: only non-test Go files directly in a SAFETY directory", () => {
  const files = parseDiff(
    [
      "diff --git a/internal/ota/a.go b/internal/ota/a.go",
      "--- a/internal/ota/a.go",
      "+++ b/internal/ota/a.go",
      "@@ -1 +1 @@",
      "+x",
      "diff --git a/internal/ota/a_test.go b/internal/ota/a_test.go",
      "--- a/internal/ota/a_test.go",
      "+++ b/internal/ota/a_test.go",
      "@@ -1 +1 @@",
      "+x",
      "diff --git a/internal/web/w.go b/internal/web/w.go",
      "--- a/internal/web/w.go",
      "+++ b/internal/web/w.go",
      "@@ -1 +1 @@",
      "+x",
      "",
    ].join("\n"),
  );
  assert.deepEqual(
    [...changedLines(files, ["internal/ota"]).keys()],
    ["internal/ota/a.go"],
  );
});

test("topLevelFuncs finds the end of a gofmt'ed function and a one-line function", () => {
  const f = topLevelFuncs([
    "package p",
    "func TestA(t *testing.T) {",
    "\tx()",
    "}",
    "func helper() {}",
    "func TestB(t *testing.T) {",
    "}",
  ]);
  assert.deepEqual(
    f.map((x) => [x.name, x.start, x.end]),
    [
      ["TestA", 2, 4],
      [null, 5, 5],
      ["TestB", 6, 7],
    ],
  );
});

test("planRevert: names the changed tests; a changed helper or import runs the whole package; comments change nothing", () => {
  const src = [
    "package p",
    'import "testing"',
    "// doc",
    "func TestA(t *testing.T) {",
    "\tx()",
    "}",
    "func TestB(t *testing.T) {",
    "\ty()",
    "}",
  ].join("\n");
  const mk = (ranges) =>
    new Map([
      [
        "p/a_test.go",
        { path: "p/a_test.go", status: "modified", added: ranges },
      ],
    ]);
  assert.deepEqual(planRevert(mk([[5, 5]]), () => src).runs, [
    { dir: "p", names: ["TestA"] },
  ]);
  assert.deepEqual(planRevert(mk([[3, 3]]), () => src).runs, []); // a doc comment
  assert.deepEqual(planRevert(mk([[2, 2]]), () => src).runs, [
    { dir: "p", names: null },
  ]); // import
});

test("verdict: failing test or build failure passes; all green fails; a test that never ran is an error", () => {
  const plan = { prod: [], runs: [{ dir: "p", names: ["TestA"] }] };
  const ev = (...e) => e.map((x) => ({ Package: "m/p", ...x }));
  assert.equal(
    verdict(
      plan,
      ev({ Test: "TestA", Action: "fail" }, { Action: "fail" }),
      "m",
    ).status,
    "ok",
  );
  assert.equal(verdict(plan, ev({ Action: "fail" }), "m").status, "ok"); // does not build without the change
  assert.equal(
    verdict(
      plan,
      ev({ Test: "TestA", Action: "pass" }, { Action: "pass" }),
      "m",
    ).status,
    "fail",
  );
  assert.equal(verdict(plan, ev({ Action: "pass" }), "m").status, "error"); // "no tests to run"
});

test("verdict: a selected test that skipped (no decoder, quarantine) proves nothing: an error, never 'passes either way'", () => {
  const plan = { prod: [], runs: [{ dir: "p", names: ["TestA", "TestB"] }] };
  const ev = (...e) => e.map((x) => ({ Package: "m/p", ...x }));
  const v = verdict(
    plan,
    ev(
      { Test: "TestA", Action: "skip" },
      { Test: "TestB", Action: "pass" },
      { Action: "pass" },
    ),
    "m",
  );
  assert.equal(v.status, "error");
  assert.deepEqual(v.runs[0].skipped, ["TestA"]);
  assert.deepEqual(v.runs[0].passed, ["TestB"]);
  // another changed test that fails without the change still decides it
  assert.equal(
    verdict(
      plan,
      ev(
        { Test: "TestA", Action: "skip" },
        { Test: "TestB", Action: "fail" },
        { Action: "fail" },
      ),
      "m",
    ).status,
    "ok",
  );
});

test("optOut: label, or a marker with a reason; the template's comment and a bare marker do not count", () => {
  assert.equal(
    optOut({ labels: [{ name: "no-revert-check" }], body: "" }).source,
    'label "no-revert-check"',
  );
  assert.equal(
    optOut({ body: "x\nrevert-check: skip - pure refactor\n" }).reason,
    "pure refactor",
  );
  assert.equal(
    optOut({ body: "revert-check: skip (new feature)" }).reason,
    "new feature",
  );
  assert.equal(optOut({ body: "revert-check: skip" }), null);
  assert.equal(
    optOut({ body: "<!-- write revert-check: skip - <reason> -->" }),
    null,
  );
  // the marker on a line of its own inside a template comment does not count either
  assert.equal(
    optOut({ body: "<!--\nrevert-check: skip - template example\n-->" }),
    null,
  );
  assert.equal(optOut({ body: "", labels: [] }), null);
});

// ---- the gates, on a toy module ------------------------------------------------------------------------

const roots = [];
after(() => roots.forEach((r) => rmSync(r, { recursive: true, force: true })));

const git = (cwd, ...a) =>
  execFileSync(
    "git",
    [
      "-c",
      "user.name=t",
      "-c",
      "user.email=t@t",
      "-c",
      "commit.gpgsign=false",
      ...a,
    ],
    { cwd, encoding: "utf8" },
  );

function put(root, path, text) {
  mkdirSync(dirname(join(root, path)), { recursive: true });
  writeFileSync(join(root, path), text);
}

const OTA = `package ota

// Clamp limits n to 0..max.
func Clamp(n, max int) int {
	if n > max {
		return max
	}
	return n
}
`;
const OTA_TEST = `package ota

import "testing"

func TestClampAbove(t *testing.T) {
	if Clamp(9, 5) != 5 {
		t.Fatal("above max")
	}
}
`;

// A toy repository: main has the package internal/ota (a SAFETY directory) and internal/other; branch "work" is
// checked out. The gates are the real scripts, copied in.
function toy() {
  const root = mkdtempSync(join(tmpdir(), "pr-gates-"));
  roots.push(root);
  git(root, "init", "-q", "-b", "main");
  put(root, "go.mod", "module example.test/toy\n\ngo 1.21\n");
  put(root, "internal/ota/ota.go", OTA);
  put(root, "internal/ota/ota_test.go", OTA_TEST);
  put(
    root,
    "internal/other/other.go",
    "package other\n\nfunc One() int { return 1 }\n",
  );
  // the real gates are part of main, so they are not "changes" of the branch
  mkdirSync(join(root, "tools"));
  mkdirSync(join(root, "scripts"));
  for (const f of ["pr-diff.sh", "diff-cover.sh", "revert-check.sh"])
    copyFileSync(join(repo, "tools", f), join(root, "tools", f));
  for (const f of readdirSync(join(repo, "scripts")).filter((x) =>
    /^(diff-parse|diff-cover|revert-check)\.mjs$/.test(x),
  ))
    copyFileSync(join(repo, "scripts", f), join(root, "scripts", f));
  git(root, "add", "-A");
  git(root, "commit", "-q", "-m", "base");
  git(root, "checkout", "-q", "-b", "work");
  return root;
}

const run = (root, script, env = {}) =>
  spawnSync("sh", [script], {
    cwd: root,
    encoding: "utf8",
    env: {
      ...process.env,
      BASE: "main",
      SAFETY: "./internal/ota",
      NICE: "",
      GOFLAGS: "",
      ...env,
    },
  });
const out = (r) => r.stdout + r.stderr;

const FIXED = OTA.replace(
  "if n > max {",
  "if n < 0 {\n\t\treturn 0\n\t}\n\tif n > max {",
);

test("diff-cover fails on a changed SAFETY line no test runs and names it", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  const r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 1, out(r));
  assert.match(out(r), /internal\/ota\/ota\.go:6\b/);
  assert.match(out(r), /internal\/ota\/ota\.go:7\b/);
});

test("diff-cover passes when a test runs the changed lines, and for a comment-only change", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampBelow(t *testing.T) {
	if Clamp(-3, 5) != 0 {
		t.Fatal("below zero")
	}
}
`,
  );
  let r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 0, out(r));
  assert.match(out(r), /every changed line is executed/);
  git(root, "checkout", "--", ".");
  put(
    root,
    "internal/ota/ota.go",
    OTA.replace("// Clamp limits", "// Clamp limits (changed comment)"),
  );
  r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 0, out(r));
});

test("diff-cover checks an untracked new file, and ignores a package outside SAFETY", () => {
  const root = toy();
  put(
    root,
    "internal/ota/extra.go",
    "package ota\n\nfunc Extra() int {\n\treturn 7\n}\n",
  );
  const r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 1, out(r));
  assert.match(out(r), /internal\/ota\/extra\.go:4/);
  rmSync(join(root, "internal/ota/extra.go"));
  put(
    root,
    "internal/other/other.go",
    "package other\n\nfunc One() int { return 2 }\nfunc Two() int { return 3 }\n",
  );
  const r2 = run(root, "tools/diff-cover.sh");
  assert.equal(r2.status, 0, out(r2));
  assert.match(out(r2), /no line of a SAFETY package changed/);
});

test("diff-cover passes a file of types and constants, and fails a file no test binary compiles", () => {
  const root = toy();
  put(
    root,
    "internal/ota/kinds.go",
    'package ota\n\n// Kind of a job.\ntype Kind string\n\n// Kinds.\nconst (\n\tKindA Kind = "a"\n)\n',
  );
  let r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 0, out(r));
  put(
    root,
    "internal/ota/never.go",
    "//go:build tasmofleet_never\n\npackage ota\n\nfunc Never() int {\n\treturn 1\n}\n",
  );
  r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 1, out(r));
  assert.match(out(r), /internal\/ota\/never\.go:6 \(no test binary compiled/);
  assert.doesNotMatch(out(r), /kinds\.go/);
});

test("diff-cover counts the tests tagged integration (the ota engine is exercised by them)", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/below_integration_test.go",
    '//go:build integration\n\npackage ota\n\nimport "testing"\n\nfunc TestClampBelowIntegration(t *testing.T) {\n\tif Clamp(-3, 5) != 0 {\n\t\tt.Fatal("below zero")\n\t}\n}\n',
  );
  const r = run(root, "tools/diff-cover.sh");
  assert.equal(r.status, 0, out(r));
});

test("revert check passes for a fix whose test fails without it, and leaves the checkout alone", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampBelowZero(t *testing.T) {
	if Clamp(-3, 5) != 0 {
		t.Fatal("below zero")
	}
}
`,
  );
  const before =
    git(root, "status", "--short") +
    readFileSync(join(root, "internal/ota/ota.go"), "utf8");
  const r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 0, out(r));
  assert.match(out(r), /FAILS without the change: TestClampBelowZero/);
  assert.match(
    out(r),
    /revert check passed: the changed tests fail without the production changes/,
  );
  assert.equal(
    git(root, "status", "--short") +
      readFileSync(join(root, "internal/ota/ota.go"), "utf8"),
    before,
  );
});

test("revert check fails for a fix whose new test also passes without the fix", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampInRange(t *testing.T) {
	if Clamp(3, 5) != 3 {
		t.Fatal("in range")
	}
}
`,
  );
  const r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 1, out(r));
  assert.match(out(r), /passes either way:\s+TestClampInRange/);
  assert.match(out(r), /revert-check: skip - <reason>/);
});

test("revert check reports a changed test that skips as skipped (it tests nothing), not as passing either way", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampBelowZero(t *testing.T) {
    t.Skip("DECODER_PATH not set")
}
`,
  );
  const r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 2, out(r));
  assert.match(out(r), /SKIPPED, tests nothing:\s+TestClampBelowZero/);
  assert.doesNotMatch(out(r), /passes either way/);
});

test("revert check puts a renamed and fixed production file back under its old name (a move is a delete and an add)", () => {
  const root = toy();
  git(root, "mv", "internal/ota/ota.go", "internal/ota/clamp.go");
  put(root, "internal/ota/clamp.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampBelowZero(t *testing.T) {
    if Clamp(-3, 5) != 0 {
        t.Fatal("below zero")
    }
}
`,
  );
  const r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 0, out(r));
  assert.match(out(r), /FAILS without the change: TestClampBelowZero/);
});

test("revert check: the opt-out is stated in the output; a feature that does not build without it passes", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST +
      `
func TestClampInRange(t *testing.T) {
	if Clamp(3, 5) != 3 {
		t.Fatal("in range")
	}
}
`,
  );
  const r = run(root, "tools/revert-check.sh", {
    REVERT_CHECK_SKIP: "pure refactor",
  });
  assert.equal(r.status, 0, out(r));
  assert.match(
    out(r),
    /revert check SKIPPED \(opt-out: REVERT_CHECK_SKIP\): pure refactor/,
  );
  git(root, "checkout", "--", ".");
  put(
    root,
    "internal/ota/feature.go",
    "package ota\n\nfunc Double(n int) int { return 2 * n }\n",
  );
  put(
    root,
    "internal/ota/feature_test.go",
    'package ota\n\nimport "testing"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal("double")\n\t}\n}\n',
  );
  const r2 = run(root, "tools/revert-check.sh");
  assert.equal(r2.status, 0, out(r2));
  assert.match(out(r2), /fails \(build error, panic or timeout\)/);
  // a reviewer must see that no test failed: the pass proves only that the tests use the new API
  assert.match(out(r2), /passed only because the tests do not build/);
});

test("revert check has nothing to do for production-only or test-only changes", () => {
  const root = toy();
  put(root, "internal/ota/ota.go", FIXED);
  let r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 0, out(r));
  assert.match(out(r), /no Go test was added or changed/);
  git(root, "checkout", "--", ".");
  put(
    root,
    "internal/ota/ota_test.go",
    OTA_TEST + "\nfunc TestMore(t *testing.T) {}\n",
  );
  r = run(root, "tools/revert-check.sh");
  assert.equal(r.status, 0, out(r));
  assert.match(out(r), /no production Go file changed/);
});
