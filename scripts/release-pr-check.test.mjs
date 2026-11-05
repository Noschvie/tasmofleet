// make release-pr-check (#556): the release commit rides in the merge train, so the tag's guards run in the PR
// that adds the new "## X.Y.Z" section. These tests fix what the PR gate must catch BEFORE a tag exists.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  copyFileSync,
  mkdirSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");
const dir = mkdtempSync(join(tmpdir(), "release-pr-check-"));
after(() => rmSync(dir, { recursive: true, force: true }));

const git = (...a) =>
  execFileSync(
    "git",
    [
      "-c",
      "commit.gpgsign=false",
      "-c",
      "tag.gpgsign=false",
      "-c",
      "user.name=t",
      "-c",
      "user.email=t@t",
      ...a,
    ],
    {
      cwd: dir,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" },
    },
  );
const w = (f, s) => {
  mkdirSync(dirname(join(dir, f)), { recursive: true });
  writeFileSync(join(dir, f), s);
};
const check = (base = "main") =>
  spawnSync("make", ["-s", "release-pr-check", `BASE=${base}`], {
    cwd: dir,
    encoding: "utf8",
  });

git("init", "-q", "-b", "main");
copyFileSync(join(repo, "Makefile"), join(dir, "Makefile"));
w("CHANGELOG.md", '## Unreleased\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n');
w("internal/store/migrations/0001_init.sql", "create table a(x int);\n");
git("add", "-A");
git("commit", "-q", "-m", "base");
git("tag", "v0.1.0");

test("a PR without a new release section passes and checks nothing", () => {
  git("switch", "-q", "-c", "plain");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  git("commit", "-q", "-am", "plain");
  const r = check();
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /no new release section/);
});

test("a release commit with a well-formed section passes both tag guards", () => {
  git("switch", "-q", "main");
  git("switch", "-q", "-c", "ok");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.1.1 "N" (2026-01-02)\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  git("commit", "-q", "-am", "release 0.1.1");
  const r = check();
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /new section 0\.1\.1, checking tag v0\.1\.1/);
});

test("a section heading the tag run would reject fails in the PR", () => {
  git("switch", "-q", "main");
  git("switch", "-q", "-c", "badheading");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.1.1 - 2026-01-02\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  git("commit", "-q", "-am", "release 0.1.1");
  assert.notEqual(check().status, 0);
});

test("a patch release that changes a migration fails in the PR", () => {
  git("switch", "-q", "main");
  git("switch", "-q", "-c", "migration");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.1.1 "N" (2026-01-02)\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  w("internal/store/migrations/0002_new.sql", "create table b(y int);\n");
  git("add", "-A");
  git("commit", "-q", "-m", "release 0.1.1 with a migration");
  const r = check();
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /migration/i);
});

test("a minor release may add a migration", () => {
  git("switch", "-q", "main");
  git("switch", "-q", "-c", "minor");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.2.0 "N" (2026-01-02)\n\n- a feature\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  w("internal/store/migrations/0002_new.sql", "create table b(y int);\n");
  git("add", "-A");
  git("commit", "-q", "-m", "release 0.2.0");
  const r = check();
  assert.equal(r.status, 0, r.stderr);
});

// "required" gates every merge: fixing the date or name of a patch release that shipped before a later minor
// added a migration is no release, and the patch guard of that old tag would fail on the newer migration.
test("editing the heading of an old patch release checks nothing", () => {
  git("switch", "-q", "main");
  git("switch", "-q", "-c", "history");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.1.1 "N" (2026-01-02)\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  git("commit", "-q", "-am", "release 0.1.1");
  git("tag", "v0.1.1");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.2.0 "M" (2026-01-03)\n\n- a feature\n\n## 0.1.1 "N" (2026-01-02)\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  w("internal/store/migrations/0002_new.sql", "create table b(y int);\n");
  git("add", "-A");
  git("commit", "-q", "-m", "release 0.2.0");
  git("tag", "v0.2.0");
  git("switch", "-q", "-c", "fixdate");
  w(
    "CHANGELOG.md",
    '## Unreleased\n\n## 0.2.0 "M" (2026-01-03)\n\n- a feature\n\n## 0.1.1 "N" (2026-01-04)\n\n- a fix\n\n## 0.1.0 "N" (2026-01-01)\n\nfirst\n',
  );
  git("commit", "-q", "-am", "changelog: the date of 0.1.1");
  const r = check("history");
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /no new release section/);
});

// Fail closed like the "changes" job of pr.yml: a base git cannot resolve (a typo, a ref never fetched) must not
// pass as "no new release section".
test("a BASE that is not a commit fails instead of passing", () => {
  const r = check("no-such-branch");
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /not a commit/);
});
