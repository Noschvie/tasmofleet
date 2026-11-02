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
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  copyFileSync,
  chmodSync,
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

function newModule() {
  const dir = mkdtempSync(join(tmpdir(), "mutation-diff-"));
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
  assert.match(r.out, /no mutant was decided on the changed lines/);
});

test("a change that only touches the tests mutates the whole package", () => {
  const m = newModule();
  m.edit("internal/p/p_test.go", "0: 0,", "0: 0, 3: 4,");
  const r = m.run({ MUTATION_DIFF: "main" });
  assert.match(r.out, /no non-test file of \.\/internal\/p changed/);
  assert.ok(r.mutants > 3, r.out);
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
  const dir = mkdtempSync(join(tmpdir(), "mutation-sum-"));
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

test("the whole package still fails when no mutant was decided, a diff passes", () => {
  assert.equal(sum(["0 0 0 12 package", "0 0 0 0 package"]).status, 1);
  const r = sum(["0 0 0 2 diff", "0 0 0 0 diff"]);
  assert.equal(r.status, 0, r.out);
  assert.match(
    r.out,
    /no mutant was decided on the changed lines; 2 not covered/,
  );
});

test("a diff of fewer than 10 mutants may keep one lived mutant but needs a kill", () => {
  assert.equal(sum(["3 1 0 0 diff"]).status, 0); // 75 % < 85 % in a package of 4 mutants: noise
  assert.equal(sum(["1 2 0 0 diff"]).status, 1); // two lived mutants in a handful
  assert.equal(sum(["0 1 0 0 diff"]).status, 1); // the only mutant lived: the line is not tested
  assert.equal(sum(["2 0 1 0 diff"]).status, 0); // a timed-out mutant in a handful is not a verdict
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
  const dir = mkdtempSync(join(tmpdir(), "mutation-report-"));
  mkdirSync(join(dir, "mutation-counts-0"));
  writeFileSync(join(dir, "mutation-counts-0/shard"), "1/4\n");
  const rep = spawnSync("sh", [join(repo, "tools/mutation-report.sh"), dir, "./internal/p:50"], { encoding: "utf8" });
  assert.equal(rep.status, 0, rep.stderr);
  assert.deepEqual(JSON.parse(readFileSync(join(dir, "mutation-report.json"), "utf8")).packages, []);
});
