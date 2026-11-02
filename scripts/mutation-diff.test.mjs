// Mutation testing of a pull request mutates the lines it changed, not the whole package (#475). That
// is only safe when no changed line can slip through unmutated, so these tests run the real gremlins
// (pinned in tools/go.mod) through tools/mutation-gate.sh on a toy module and count the mutants:
//
//  - gremlins --diff with a package path argument matches no line at all (its positions are
//    package-relative, the diff's are repo-relative): every mutant is skipped and the gate would pass
//    without testing anything;
//  - with git's default 3 lines of context gremlins drops the second changed line of a hunk that holds
//    two changes;
//  - git diff does not list an untracked file; a change that only touches tests mutates nothing in the
//    diff, yet it can weaken the tests, so it keeps the whole-package run.
//
// The thresholds of the sum over shards (--sum) are tested with made-up counts.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  copyFileSync,
  chmodSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");
const gate = join(repo, "tools/mutation-gate.sh");

const source = `package p

func A(x int) int {
	if x > 1 {
		return x + 1
	}
	if x < -1 {
		return x - 1
	}
	return x * 3
}
`;
const tests = `package p

import "testing"

func TestA(t *testing.T) {
	for in, want := range map[int]int{5: 6, 2: 3, -2: -3, -5: -6, 0: 0, 1: 3, -1: -3} {
		if got := A(in); got != want {
			t.Errorf("A(%d) = %d, want %d", in, got, want)
		}
	}
}
`;

// B is reached by no test (its mutants are "not covered"), C is called but never checked (its mutants live).
const notTested = `package p

func B(x int) int {
	if x > 3 {
		return x + 1
	}
	return 0
}
`;
const weaklyTested = `package p

func C(x int) int {
	if x > 3 {
		return x + 1
	}
	return 0
}
`;
const weakTest = `package p

import "testing"

func TestC(t *testing.T) { _ = C(5) }
`;

// Every temporary directory of this file, removed when the tests are done (a seed alone is 69 MB).
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});
function mkTmp(prefix) {
  const d = mkdtempSync(join(tmpdir(), prefix));
  made.push(d);
  return d;
}

// The gate runs gremlins with a throwaway GOCACHE (#546). A cold one rebuilds the standard library for every
// run (2 s on a fast machine, much more on a CI runner), so all runs start from one seed built here once.
let seedDir;
function gocacheSeed() {
  if (seedDir) return seedDir;
  seedDir = mkTmp("mutation-diff-seed-");
  const mod = mkTmp("mutation-diff-seedmod-");
  writeFileSync(join(mod, "go.mod"), "module example.com/gm\n\ngo 1.22\n");
  writeFileSync(join(mod, "p.go"), "package p\n");
  writeFileSync(
    join(mod, "p_test.go"),
    'package p\n\nimport "testing"\n\nfunc TestP(t *testing.T) {}\n',
  );
  execFileSync("go", ["test", "-cover", "./..."], {
    cwd: mod,
    env: { ...process.env, GOCACHE: seedDir },
  });
  return seedDir;
}

function newModule() {
  const dir = mkTmp("mutation-diff-");
  // No global git config: the throw-away repository needs neither the user's signing key nor identity.
  const env = {
    ...process.env,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_SYSTEM: "/dev/null",
  };
  const git = (...args) =>
    execFileSync(
      "git",
      ["-c", "user.name=t", "-c", "user.email=t@t", ...args],
      {
        cwd: dir,
        env,
        encoding: "utf8",
      },
    );
  git("init", "-q", "-b", "main");
  mkdirSync(join(dir, "tools"));
  copyFileSync(
    join(repo, "tools/changed-files.sh"),
    join(dir, "tools/changed-files.sh"),
  );
  chmodSync(join(dir, "tools/changed-files.sh"), 0o755);
  mkdirSync(join(dir, "internal/p"), { recursive: true });
  writeFileSync(join(dir, "go.mod"), "module example.com/gm\n\ngo 1.22\n");
  writeFileSync(join(dir, "internal/p/p.go"), source);
  writeFileSync(join(dir, "internal/p/p_test.go"), tests);
  writeFileSync(join(dir, "internal/p/b.go"), notTested);
  writeFileSync(join(dir, "internal/p/c.go"), weaklyTested);
  writeFileSync(join(dir, "internal/p/c_test.go"), weakTest);
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  git("switch", "-q", "-c", "work");
  const write = (f, s) => writeFileSync(join(dir, f), s);
  const edit = (f, from, to) =>
    write(f, readFileSync(join(dir, f), "utf8").replace(from, to));
  // Runs the real gate on ./internal/p in diff scope against main (or the whole package).
  const run = (extra = {}) => {
    const r = spawnSync("sh", [gate, "./internal/p", "50"], {
      cwd: dir,
      env: {
        ...env,
        TOOL: `go tool -modfile=${repo}/tools/go.mod`,
        MUTATION_WORKERS: "2",
        MUTATION_GOCACHE_SEED: gocacheSeed(),
        ...extra,
      },
      encoding: "utf8",
    });
    const out = r.stdout + r.stderr;
    const m = out.match(/Killed: (\d+), Lived: (\d+)/);
    return {
      status: r.status,
      out,
      mutants: m ? Number(m[1]) + Number(m[2]) : 0,
    };
  };
  return { dir, git, write, edit, run };
}

test("two changed lines inside one hunk are both mutated, one committed and one not", () => {
  const m = newModule();
  m.edit("internal/p/p.go", "return x + 1", "return 1 + x");
  m.git("commit", "-q", "-am", "committed change");
  m.edit("internal/p/p.go", "return x - 1", "return x + (-1)"); // uncommitted, 3 lines below
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /diff scope/);
  // 1 + x has one mutant, x + (-1) three (two arithmetic, one negation): 4. With git's default 3 lines of
  // context gremlins sees only the first line (1), with a package path argument none (0).
  assert.equal(r.mutants, 4, r.out);
});

test("a diff without a mutable line passes and says so (a comment is no safety change)", () => {
  const m = newModule();
  m.edit(
    "internal/p/p.go",
    "func A(",
    "// A doubles, except around zero.\nfunc A(",
  );
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /no mutant on the changed lines/);
});

test("a change that only touches the tests mutates the whole package", () => {
  const m = newModule();
  m.edit("internal/p/p_test.go", "0: 0,", "0: 0, 3: 4,");
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /no non-test Go file of \.\/internal\/p changed/);
  assert.ok(r.mutants > 3, r.out);
});

test("a code change with a test change in the same package mutates the whole package", () => {
  // the tests may have got weaker while a line changed: the changed line alone would hide it
  const m = newModule();
  m.edit("internal/p/p.go", "return x + 1", "return 1 + x");
  m.edit("internal/p/p_test.go", "0: 0,", "0: 0, 3: 4,");
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /tests or data of \.\/internal\/p changed too/);
  assert.ok(r.mutants > 4, r.out);
});

test("a code change with changed test data in the same package mutates the whole package", () => {
  const m = newModule();
  mkdirSync(join(m.dir, "internal/p/testdata"));
  m.write("internal/p/testdata/case.txt", "x\n");
  m.git("add", "-A");
  m.git("commit", "-q", "-m", "data");
  m.git("switch", "-q", "main");
  m.git("merge", "-q", "--ff-only", "work");
  m.git("switch", "-q", "-c", "work2");
  m.write("internal/p/testdata/case.txt", "y\n");
  m.edit("internal/p/p.go", "return x + 1", "return 1 + x");
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /tests or data of \.\/internal\/p changed too/);
  assert.ok(r.mutants > 4, r.out);
});

test("a changed line that no test reaches fails the diff gate", () => {
  const m = newModule();
  m.edit("internal/p/b.go", "return x + 1", "return 1 + x"); // B has no test: its mutants are not covered
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /0 killed, 1 lived or not covered of the 1 mutants on the changed lines/);
});

test("an untracked source file mutates the whole package, git diff cannot see it", () => {
  const m = newModule();
  m.edit("internal/p/p.go", "return x + 1", "return 1 + x");
  m.write(
    "internal/p/q.go",
    "package p\n\nfunc D(x int) int { return x + 1 }\n",
  );
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /untracked file/);
  assert.ok(r.mutants > 3, r.out);
});

test("without MUTATION_DIFF the whole package is mutated, as in the nightly", () => {
  const m = newModule();
  const r = m.run();
  assert.match(r.out, /package scope/);
  assert.ok(r.mutants > 3, r.out);
});

// The sum over the shards' counts: "killed lived timedout notcovered scope" per shard.
function sum(lines, min = 85) {
  const dir = mkTmp("mutation-sum-");
  const files = lines.map((l, i) => {
    const f = join(dir, `shard${i}`);
    writeFileSync(f, `${l}\n`);
    return f;
  });
  const r = spawnSync(
    "sh",
    [gate, "--sum", "./internal/p", String(min), ...files],
    {
      encoding: "utf8",
    },
  );
  return { status: r.status, out: r.stdout + r.stderr };
}

test("the whole package still fails when no mutant was decided, a diff with no mutant at all passes", () => {
  assert.equal(sum(["0 0 0 12 package", "0 0 0 0 package"]).status, 1);
  const r = sum(["0 0 0 0 diff", "0 0 0 0 diff"]);
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /no mutant on the changed lines/);
});

test("on the changed lines of a diff a mutant no test reaches counts as lived", () => {
  // a changed line that no test runs must not pass (it passed while only killed and lived counted)
  assert.equal(sum(["0 0 0 2 diff", "0 0 0 0 diff"]).status, 1);
  assert.equal(sum(["1 0 0 20 diff"]).status, 1);
  assert.equal(sum(["2 0 0 1 diff"]).status, 0); // one in a handful is tolerated, like a lived one
  assert.equal(sum(["2 0 0 2 diff"]).status, 1);
  assert.equal(sum(["9 0 0 1 diff"]).status, 0); // 9 of 10 at 85 %
  assert.equal(sum(["8 0 0 3 diff"]).status, 1); // 8 of 11
  // in a whole package, as ever, the mutants nobody reaches stay out of the efficacy
  assert.equal(sum(["8 1 0 50 package"]).status, 0);
});

test("a diff of fewer than 10 mutants may keep one lived mutant but needs a kill", () => {
  assert.equal(sum(["3 1 0 0 diff"]).status, 0); // 75 % < 85 % in a package of 4 mutants: noise
  assert.equal(sum(["1 2 0 0 diff"]).status, 1); // two lived mutants in a handful
  assert.equal(sum(["0 1 0 0 diff"]).status, 1); // the only mutant lived: the line is not tested
  assert.equal(sum(["2 0 1 0 diff"]).status, 0); // a timed-out mutant in a handful is not a verdict
});

test("the timeout share of a diff is judged from 10 run mutants on, the percentage from 10 decided", () => {
  assert.equal(sum(["9 0 5 0 diff"]).status, 1); // 9 decided (a handful), but 5 of 14 run mutants timed out
  assert.equal(sum(["9 0 2 0 diff"]).status, 0); // 2 of 11 is within 20 %
});

test("a diff of 10 or more mutants is judged by the package's efficacy", () => {
  assert.equal(sum(["8 1 0 0 diff", "2 0 0 0 diff"]).status, 0); // 10 of 11
  assert.equal(sum(["7 3 0 0 diff", "2 0 0 0 diff"]).status, 1); // 75 %
  assert.equal(sum(["7 3 0 0 package", "2 0 0 0 package"], 75).status, 0);
});

test("a sharded run records every unkilled mutant, and the report shows them with the efficacy", () => {
  const m = newModule();
  mkdirSync(join(m.dir, "counts/mutation-counts-0"), { recursive: true });
  copyFileSync(join(repo, "tools/mutation-shard.sh"), join(m.dir, "tools/mutation-shard.sh"));
  const out = join(m.dir, "counts/mutation-counts-0/p");
  const r = m.run({ MUTATION_SHARD: "1/1", MUTATION_PKGS: "./internal/p", MUTATION_COUNTS_OUT: out });
  assert.equal(r.status, 0, r.out);
  const findings = readFileSync(`${out}.findings`, "utf8").trim().split("\n");
  // Package-relative in gremlins' output, repository-relative here, with the status and the mutator.
  assert.ok(findings.includes("LIVED|CONDITIONALS_NEGATION|internal/p/c.go|4"), findings.join("\n"));
  assert.ok(findings.includes("NOT COVERED|ARITHMETIC_BASE|internal/p/b.go|5"), findings.join("\n"));
  assert.match(readFileSync(out, "utf8").trim(), /^\d+ [1-9]\d* 0 [1-9]\d* package$/);

  const summary = join(m.dir, "summary.md");
  const rep = spawnSync("sh", [join(repo, "tools/mutation-report.sh"), join(m.dir, "counts"), "./internal/p:50", "./internal/q:50"], {
    env: { ...process.env, GITHUB_STEP_SUMMARY: summary, GITHUB_SHA: "abc123" },
    encoding: "utf8",
  });
  assert.equal(rep.status, 0, rep.stderr);
  const report = JSON.parse(readFileSync(join(m.dir, "counts/mutation-report.json"), "utf8"));
  assert.equal(report.commit, "abc123");
  assert.equal(report.packages.length, 1, "internal/q has no counts: it was not run");
  const [p] = report.packages;
  assert.equal(p.package, "internal/p");
  assert.equal(p.scope, "package");
  assert.equal(p.min_efficacy, 50);
  assert.ok(p.efficacy > 50 && p.efficacy < 100, String(p.efficacy));
  assert.ok(p.findings.some((f) => f.status === "LIVED" && f.file === "internal/p/c.go" && f.line === 4));
  assert.ok(p.findings.some((f) => f.status === "NOT COVERED" && f.file === "internal/p/b.go" && f.line === 5));
  const md = readFileSync(summary, "utf8");
  assert.match(md, /\| `internal\/p` \| package \| \d+\.\d %/);
  assert.match(md, /`internal\/p\/c\.go:4`/);
  assert.match(md, /`internal\/p\/b\.go:5`/);
});

test("the report of a run without mutation packages is empty but valid", () => {
  const dir = mkTmp("mutation-report-");
  mkdirSync(join(dir, "mutation-counts-0"));
  writeFileSync(join(dir, "mutation-counts-0/shard"), "1/4\n");
  const rep = spawnSync("sh", [join(repo, "tools/mutation-report.sh"), dir, "./internal/p:50"], { encoding: "utf8" });
  assert.equal(rep.status, 0, rep.stderr);
  assert.deepEqual(JSON.parse(readFileSync(join(dir, "mutation-report.json"), "utf8")).packages, []);
});
