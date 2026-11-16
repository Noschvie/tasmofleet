// A pull request must run every e2e spec file it changes in full, not only the tests tagged @core (#820).
// Why it matters: a file can be broken as a whole while its @core tests pass. #796: a @core test left a
// planned update behind and the next test of the same file failed, which only the nightly saw, after the
// merge. So "make e2e-core BASE=<ref>" runs a second Playwright pass over the spec files changed since the
// merge base with <ref>, in both projects, next to the @core pass.
//
// The test runs the real Makefile and tools/changed-files.sh in a throw-away repository; npx is a stub on
// PATH that prints what Playwright would have been asked to do, and "make -o build" skips the Go build.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import {
  mkdtempSync,
  rmSync,
  mkdirSync,
  writeFileSync,
  copyFileSync,
  chmodSync,
  renameSync,
  unlinkSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");

// Every throw-away repository is removed when the file is done (#552).
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

function newRepo() {
  const dir = mkdtempSync(join(tmpdir(), "e2e-changed-"));
  made.push(dir);
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
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
  git("init", "-q", "-b", "main");
  copyFileSync(join(repo, "Makefile"), join(dir, "Makefile"));
  mkdirSync(join(dir, "tools"));
  copyFileSync(
    join(repo, "tools/changed-files.sh"),
    join(dir, "tools/changed-files.sh"),
  );
  chmodSync(join(dir, "tools/changed-files.sh"), 0o755);
  // The stub Playwright: prints its arguments; fails when asked to (the @core pass only).
  mkdirSync(join(dir, "stub"));
  writeFileSync(
    join(dir, "stub/npx"),
    '#!/bin/sh\necho "npx $*"\n' +
      'case "$*" in *"--grep @core"*) [ -z "${STUB_CORE_FAILS:-}" ] || exit 1;; esac\n',
  );
  chmodSync(join(dir, "stub/npx"), 0o755);
  mkdirSync(join(dir, "e2e/tests"), { recursive: true });
  for (const f of ["x", "y", "mqtt-transport", "gone", "old-name"])
    writeFileSync(join(dir, `e2e/tests/${f}.spec.ts`), "// base\n");
  writeFileSync(join(dir, "e2e/support.ts"), "// base\n");
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  git("switch", "-q", "-c", "work");
  const run = (extra = [], envExtra = {}) => {
    const p = spawnSync(
      "make",
      ["-s", "-o", "build", "e2e-core", "NICE=", ...extra],
      {
        cwd: dir,
        env: {
          ...env,
          ...envExtra,
          PATH: `${join(dir, "stub")}:${process.env.PATH}`,
        },
        encoding: "utf8",
      },
    );
    const calls = p.stdout.split("\n").filter((l) => l.startsWith("npx "));
    const core = calls.filter((l) => l.includes("--grep @core"));
    const changed = calls.filter((l) => !l.includes("--grep @core"));
    return { status: p.status, err: p.stderr, calls, core, changed };
  };
  const write = (f, s) => {
    mkdirSync(dirname(join(dir, f)), { recursive: true });
    writeFileSync(join(dir, f), s);
  };
  const commit = () => {
    git("add", "-A");
    git("commit", "-q", "-m", "work");
  };
  return { dir, git, run, write, commit };
}

const project = (l) => l.match(/--project=(\w+)/)[1];

test("a changed spec file runs completely in both projects, an unchanged one does not", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const res = r.run(["BASE=main"]);
  assert.equal(res.status, 0, res.err);
  assert.equal(res.core.length, 2, "the @core pass stays, once per project");
  assert.deepEqual(res.changed.map(project).sort(), ["chromium", "mqtt"]);
  for (const l of res.changed) {
    assert.match(l, /\btests\/x\.spec\.ts\b/);
    assert.doesNotMatch(l, /y\.spec\.ts/);
    assert.doesNotMatch(
      l,
      /--grep @core/,
      "the whole file, not only its @core tests",
    );
  }
});

test("an uncommitted and an untracked spec count as changed (a local run before the commit)", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// edited, not committed\n");
  r.write("e2e/tests/new.spec.ts", "// new, not added\n");
  const res = r.run(["BASE=main"]);
  assert.equal(res.status, 0, res.err);
  const c = res.changed.find((l) => project(l) === "chromium");
  assert.match(c, /tests\/x\.spec\.ts/);
  assert.match(c, /tests\/new\.spec\.ts/);
});

test("a PR that changes no spec runs the @core pass only, as before (the FAST tier stays short)", () => {
  const r = newRepo();
  r.write("e2e/support.ts", "// changed, not a spec\n");
  r.write("internal/x.go", "package x\n");
  r.commit();
  const res = r.run(["BASE=main"]);
  assert.equal(res.status, 0, res.err);
  assert.equal(res.core.length, 2);
  assert.deepEqual(res.changed, []);
});

test("without BASE (the local default) only the @core pass runs and git is not asked", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const res = r.run();
  assert.equal(res.status, 0, res.err);
  assert.equal(res.core.length, 2);
  assert.deepEqual(res.changed, []);
});

test("a renamed spec runs under its new name and a deleted one is not passed to Playwright", () => {
  const r = newRepo();
  renameSync(
    join(r.dir, "e2e/tests/old-name.spec.ts"),
    join(r.dir, "e2e/tests/new-name.spec.ts"),
  );
  unlinkSync(join(r.dir, "e2e/tests/gone.spec.ts"));
  r.commit();
  const res = r.run(["BASE=main"]);
  assert.equal(res.status, 0, res.err);
  const c = res.changed.find((l) => project(l) === "chromium");
  assert.match(c, /tests\/new-name\.spec\.ts/);
  assert.doesNotMatch(
    c,
    /old-name|gone/,
    "a path that no longer exists would run nothing or fail the shard",
  );
});

test("the second pass of the chromium project carries the shard, so the shards split the changed files", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const res = r.run(["BASE=main", "E2E_ARGS=--shard=1/2"]);
  const c = res.changed.find((l) => project(l) === "chromium");
  assert.match(
    c,
    /--shard=1\/2/,
    "without it every shard runs every changed file and the budget doubles",
  );
  const m = res.changed.find((l) => project(l) === "mqtt");
  assert.doesNotMatch(m, /--shard/, "the mqtt project is never sharded");
});

test("a quarantined test of a changed file does not block the PR", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const changed = r.run(["BASE=main"]).changed;
  assert.equal(changed.length, 2);
  for (const l of changed) assert.match(l, /--grep-invert @quarantine/);
});

test("a changed MQTT spec runs in the mqtt job, and the shards (E2E_MQTT=0) leave it alone", () => {
  const r = newRepo();
  r.write("e2e/tests/mqtt-transport.spec.ts", "// changed\n");
  r.commit();
  const mqtt = r.run(["BASE=main", "E2E_CHROMIUM=0", "E2E_MQTT=1"]);
  assert.deepEqual(mqtt.changed.map(project), ["mqtt"]);
  assert.match(mqtt.changed[0], /tests\/mqtt-transport\.spec\.ts/);
  const shard = r.run(["BASE=main", "E2E_CHROMIUM=1", "E2E_MQTT=0"]);
  assert.deepEqual(shard.changed.map(project), ["chromium"]);
});

test("a PR that has no spec to run in a project passes it --pass-with-no-tests (a shard may get none of the files)", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const changed = r.run(["BASE=main"]).changed;
  assert.equal(changed.length, 2);
  for (const l of changed) assert.match(l, /--pass-with-no-tests/);
});

test("a BASE without a merge base fails loudly instead of quietly running @core only (that is #820 again)", () => {
  const r = newRepo();
  const res = r.run(["BASE=no-such-ref"]);
  assert.notEqual(res.status, 0);
  assert.match(res.err, /e2e-core: .*no-such-ref/);
  assert.deepEqual(res.calls, [], "nothing runs on a broken checkout");
});

test("a failing @core pass still runs the changed specs, and the target fails", () => {
  const r = newRepo();
  r.write("e2e/tests/x.spec.ts", "// changed\n");
  r.commit();
  const res = r.run(["BASE=main"], { STUB_CORE_FAILS: "1" });
  assert.notEqual(res.status, 0);
  assert.equal(
    res.changed.length,
    2,
    "one red pass must not hide the failures of the other",
  );
});
