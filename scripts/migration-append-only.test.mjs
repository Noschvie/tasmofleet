import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, renameSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const script = resolve("tools/migration-append-only.sh");

// A throwaway repository with migrations 0001+0002 shipped as v0.1.0 (and 0003 as v0.2.0).
function repo() {
  const dir = mkdtempSync(join(tmpdir(), "mig-"));
  const git = (...a) => execFileSync("git", ["-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.invalid", ...a], { cwd: dir, stdio: "pipe" });
  git("init", "-q", "-b", "main");
  const m = join(dir, "internal/store/migrations");
  mkdirSync(m, { recursive: true });
  writeFileSync(join(m, "0001_init.sql"), "create table a(x int);\n");
  writeFileSync(join(m, "0002_more.sql"), "create table b(y int);\n");
  git("add", "-A"); git("commit", "-q", "-m", "one"); git("tag", "v0.1.0");
  writeFileSync(join(m, "0003_new.sql"), "create table c(z int);\n");
  git("add", "-A"); git("commit", "-q", "-m", "two"); git("tag", "v0.2.0");
  const run = (...args) => spawnSync("sh", [script, ...args], { cwd: dir, encoding: "utf8" });
  const commit = (msg) => { git("add", "-A"); git("commit", "-q", "-m", msg); };
  return { dir, m, run, commit, git, cleanup: () => rmSync(dir, { recursive: true, force: true }) };
}

// Why: a minor release that adds a migration is the normal way a schema changes; the guard must not block it.
test("adding a migration passes", () => {
  const r = repo();
  try {
    writeFileSync(join(r.m, "0004_next.sql"), "create table d(w int);\n");
    r.commit("add");
    assert.equal(r.run().status, 0);
  } finally { r.cleanup(); }
});

// Why: existing installations record only the schema version; an edited shipped file leaves them silently on the old schema.
// 0003 shipped in v0.2.0, the latest tag, so the guard must cover it (the old frozen list covered only 0001/0002).
test("editing a file shipped in the latest tag fails and names it", () => {
  const r = repo();
  try {
    writeFileSync(join(r.m, "0003_new.sql"), "create table c(z text);\n");
    r.commit("edit");
    const res = r.run();
    assert.notEqual(res.status, 0);
    assert.match(res.stderr, /0003_new\.sql/);
  } finally { r.cleanup(); }
});

test("deleting a shipped file fails", () => {
  const r = repo();
  try {
    rmSync(join(r.m, "0001_init.sql"));
    r.commit("delete");
    assert.notEqual(r.run().status, 0);
  } finally { r.cleanup(); }
});

test("renaming a shipped file fails", () => {
  const r = repo();
  try {
    renameSync(join(r.m, "0002_more.sql"), join(r.m, "0002_renamed.sql"));
    r.commit("rename");
    assert.notEqual(r.run().status, 0);
  } finally { r.cleanup(); }
});

// Why: the tag job runs with HEAD on the tag being released; comparing with that very tag would
// accept everything, so the tag itself is excluded and the previous one is the baseline.
test("TAG excludes the tag under test from the baseline", () => {
  const r = repo();
  try {
    writeFileSync(join(r.m, "0003_new.sql"), "create table c(z text);\n");
    r.commit("edit");
    r.git("tag", "v0.3.0");
    assert.equal(r.run().status, 0, "without TAG the new tag is the baseline and hides the edit");
    assert.notEqual(r.run("v0.3.0").status, 0);
  } finally { r.cleanup(); }
});

// Why: a guard that cannot find its baseline (shallow clone without tags) must fail loud, not pass.
test("no release tag fails instead of passing", () => {
  const r = repo();
  try {
    r.git("tag", "-d", "v0.1.0", "v0.2.0");
    const res = r.run();
    assert.notEqual(res.status, 0);
    assert.match(res.stderr, /no release tag/);
  } finally { r.cleanup(); }
});
