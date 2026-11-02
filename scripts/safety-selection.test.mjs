// "make safety" must select the SAFETY packages exactly as CI does (#481): what this branch changed
// since it left BASE, uncommitted and untracked work included, and nothing that BASE gained after the
// branch point. A selection that misses a package passes the local gate without testing it; one that
// over-selects makes a local run slower than the CI run it is meant to predict.
//
// The test runs the real Makefile targets in a throw-away repository; the two tools they call
// (race.sh, mutation-gate.sh) are stubs that print what they were asked to do.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  copyFileSync,
  chmodSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");

function newRepo() {
  const dir = mkdtempSync(join(tmpdir(), "safety-selection-"));
  // No global git config: the throw-away repository needs neither the user's signing key nor identity.
  const env = {
    ...process.env,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_SYSTEM: "/dev/null",
  };
  const sh = (cmd, args) =>
    execFileSync(cmd, args, {
      cwd: dir,
      env,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
  const git = (...args) =>
    sh("git", ["-c", "user.name=t", "-c", "user.email=t@t", ...args]);
  git("init", "-q", "-b", "main");
  copyFileSync(join(repo, "Makefile"), join(dir, "Makefile"));
  mkdirSync(join(dir, "tools"));
  copyFileSync(
    join(repo, "tools/changed-files.sh"),
    join(dir, "tools/changed-files.sh"),
  );
  chmodSync(join(dir, "tools/changed-files.sh"), 0o755);
  for (const [name, body] of [
    ["race.sh", 'echo "race $*"'],
    ["mutation-gate.sh", 'echo "gate $* diff=${MUTATION_DIFF:-}"'],
  ]) {
    writeFileSync(join(dir, "tools", name), `#!/bin/sh\n${body}\n`);
    chmodSync(join(dir, "tools", name), 0o755);
  }
  for (const p of ["fwselect", "ota", "tasmota", "auth", "web"]) {
    mkdirSync(join(dir, "internal", p), { recursive: true });
    writeFileSync(join(dir, "internal", p, `${p}.go`), `package ${p}\n`);
  }
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  const make = (target, extra = []) =>
    sh("make", ["-s", target, "NICE=", ...extra]);
  // The packages a target selected: the arguments of race.sh, the packages the gate was called for.
  const raced = (base) =>
    make("safety-race", [`BASE=${base}`])
      .match(/^race (.*)$/m)?.[1]
      .split(" ")
      .sort() ?? [];
  const mutated = (base) =>
    [
      ...make("mutation-changed", [`BASE=${base}`]).matchAll(
        /^gate \.\/internal\/(\w+) /gm,
      ),
    ]
      .map((m) => m[1])
      .sort();
  return {
    dir,
    git,
    make,
    raced,
    mutated,
    write: (f, s) => writeFileSync(join(dir, f), s),
  };
}

test("an uncommitted edit of a SAFETY package is selected (it was invisible before)", () => {
  const r = newRepo();
  r.git("switch", "-q", "-c", "work");
  r.write("internal/fwselect/fwselect.go", "package fwselect\n// edited\n");
  assert.deepEqual(r.raced("main"), ["./internal/fwselect"]);
  assert.deepEqual(r.mutated("main"), ["fwselect"]);
});

test("a staged edit and a new untracked file are selected", () => {
  const r = newRepo();
  r.git("switch", "-q", "-c", "work");
  r.write("internal/ota/ota.go", "package ota\n// staged\n");
  r.git("add", "internal/ota/ota.go");
  r.write("internal/tasmota/new.go", "package tasmota\n");
  assert.deepEqual(r.raced("main"), ["./internal/ota", "./internal/tasmota"]);
  assert.deepEqual(r.mutated("main"), ["ota", "tasmota"]);
});

test("changes BASE gained after the branch point are not selected", () => {
  const r = newRepo();
  r.git("switch", "-q", "-c", "work");
  r.write("internal/fwselect/fwselect.go", "package fwselect\n// mine\n");
  r.git("commit", "-q", "-am", "mine");
  r.git("switch", "-q", "main");
  r.write("internal/ota/ota.go", "package ota\n// main moved on\n");
  r.git("commit", "-q", "-am", "main moves");
  r.git("switch", "-q", "work");
  assert.deepEqual(r.raced("main"), ["./internal/fwselect"]);
  assert.deepEqual(r.mutated("main"), ["fwselect"]);
});

test("a change outside the SAFETY packages selects nothing", () => {
  const r = newRepo();
  r.git("switch", "-q", "-c", "work");
  r.write("internal/web/web.go", "package web\n// edited\n");
  assert.deepEqual(r.raced("main"), []);
  assert.deepEqual(r.mutated("main"), []);
});

test("without a usable BASE every package runs, as before", () => {
  const r = newRepo();
  assert.equal(r.raced("no-such-ref").length, 11);
  assert.equal(r.mutated("no-such-ref").length, 8);
  assert.equal(r.raced("").length, 11);
});
