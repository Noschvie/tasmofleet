// Changelog fragments (#527): every change adds changes/unreleased/<issue>-<slug>.md instead of editing
// CHANGELOG.md, so parallel PRs stop conflicting. The release commit assembles them into the version section.
// These tests fix WHY it matters: the assembled section must look exactly like the hand-written ones (heading,
// section order, bullets), the fragments must be gone afterwards, and a mistake must fail loud, not drop a line.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  existsSync,
  readdirSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const tool = join(here, "changelog-assemble.mjs");
const roots = [];
after(() => roots.forEach((d) => rmSync(d, { recursive: true, force: true })));

const HEAD = "# Changelog\n\nintro text\n\n";
const OLD = '## 0.20.1 "Tower" (2026-11-02)\n\n### Fixed\n\n- old fix\n\n## 0.20.0 "Tower" (2026-11-01)\n\n### Added\n\n- old add\n';

function repo({ changelog = HEAD + OLD, fragments = {} } = {}) {
  const d = mkdtempSync(join(tmpdir(), "changelog-assemble-"));
  roots.push(d);
  writeFileSync(join(d, "CHANGELOG.md"), changelog);
  mkdirSync(join(d, "changes/unreleased"), { recursive: true });
  for (const [n, c] of Object.entries(fragments))
    writeFileSync(join(d, "changes/unreleased", n), c);
  return d;
}
const frag = (section, body) => `---\nsection: ${section}\n---\n${body}\n`;
const run = (d, ...args) =>
  spawnSync("node", [tool, ...args], { cwd: d, encoding: "utf8" });
const assemble = (d, ...extra) =>
  run(d, "assemble", "--version", "0.21.0", "--name", "Bell Rock", "--date", "2026-11-07", ...extra);

test("assembles the section in the CHANGELOG's order, above the newest release, and deletes the fragments", () => {
  const d = repo({
    fragments: {
      "9-security.md": frag("Security", "sec line (#9)"),
      "10-fixed.md": frag("Fixed", "fix ten (#10)"),
      "2-fixed.md": frag("Fixed", "fix two (#2, thanks @bob)"),
      "30-breaking.md": frag("Breaking", "**breaking** thing (#30)"),
      "4-added.md": frag("Added", "add four (#4)"),
      "5-changed.md": frag("Changed", "change five (#5)"),
      "6-upgrade.md": frag("Upgrade notes", "Migration 0047 adds a table"),
    },
  });
  const r = assemble(d);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(
    readFileSync(join(d, "CHANGELOG.md"), "utf8"),
    HEAD +
      '## 0.21.0 "Bell Rock" (2026-11-07)\n\n' +
      "### Upgrade notes\n\n- Migration 0047 adds a table\n\n" +
      "### Added\n\n- add four (#4)\n\n" +
      "### Changed\n\n- change five (#5)\n\n" +
      "### Fixed\n\n- fix two (#2, thanks @bob)\n- fix ten (#10)\n\n" + // issue order, 2 before 10
      "### Breaking\n\n- **breaking** thing (#30)\n\n" +
      "### Security\n\n- sec line (#9)\n\n" +
      OLD,
  );
  assert.deepEqual(readdirSync(join(d, "changes/unreleased")), []);
});

test("a patch takes the name of its minor's section when NAME is omitted", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x (#1)") } });
  const r = run(d, "assemble", "--version", "0.20.2", "--date", "2026-11-07");
  assert.equal(r.status, 0, r.stderr);
  assert.match(readFileSync(join(d, "CHANGELOG.md"), "utf8"), /^## 0\.20\.2 "Tower" \(2026-11-07\)$/m);
});

test("a new minor without NAME fails and changes nothing", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x (#1)") } });
  const r = run(d, "assemble", "--version", "0.21.0", "--date", "2026-11-07");
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /NAME/);
  assert.equal(readFileSync(join(d, "CHANGELOG.md"), "utf8"), HEAD + OLD);
  assert.equal(readdirSync(join(d, "changes/unreleased")).length, 1);
});

test("the result passes the release guard (make changelog-check)", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x (#1)") } });
  assert.equal(assemble(d).status, 0);
  copyMakefile(d);
  execFileSync("make", ["-s", "changelog-check", "TAG=v0.21.0"], { cwd: d });
});
function copyMakefile(d) {
  writeFileSync(join(d, "Makefile"), readFileSync(join(here, "..", "Makefile")));
}

for (const [why, files, re] of [
  ["no fragments", {}, /no fragments/i],
  ["an unknown section", { "1-a.md": frag("Fixes", "x") }, /section/i],
  ["a missing front matter", { "1-a.md": "just text\n" }, /front matter/i],
  ["an empty body", { "1-a.md": "---\nsection: Fixed\n---\n\n" }, /empty/i],
  ["a bad file name", { "Fix A.md": frag("Fixed", "x") }, /file name/i],
]) {
  test(`fails loud and keeps everything on ${why}`, () => {
    const d = repo({ fragments: files });
    const r = assemble(d);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, re);
    assert.equal(readFileSync(join(d, "CHANGELOG.md"), "utf8"), HEAD + OLD);
    assert.equal(readdirSync(join(d, "changes/unreleased")).length, Object.keys(files).length);
  });
}

test("refuses when the version section exists or a hand-written Unreleased section is left", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x (#1)") } });
  assert.notEqual(run(d, "assemble", "--version", "0.20.1", "--date", "2026-11-07").status, 0);
  writeFileSync(join(d, "CHANGELOG.md"), HEAD + "## Unreleased\n\n### Fixed\n\n- stray\n\n" + OLD);
  const r = assemble(d);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /Unreleased/);
  assert.equal(readdirSync(join(d, "changes/unreleased")).length, 1);
});

test("a multi-line body keeps its continuation lines inside the bullet", () => {
  const d = repo({ fragments: { "1-a.md": frag("Added", "first line\nsecond line (#1)") } });
  assert.equal(assemble(d).status, 0);
  assert.match(readFileSync(join(d, "CHANGELOG.md"), "utf8"), /- first line\n {2}second line \(#1\)\n/);
});

// The PR gate: a PR that changes code must say what it changed, in a fragment or in the release section.
function gitRepo(files) {
  const d = repo();
  const git = (...a) =>
    execFileSync("git", ["-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t", ...a], {
      cwd: d, encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" },
    });
  git("init", "-q", "-b", "main");
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  git("checkout", "-q", "-b", "pr");
  for (const [p, c] of Object.entries(files)) {
    mkdirSync(dirname(join(d, p)), { recursive: true });
    writeFileSync(join(d, p), c);
  }
  git("add", "-A");
  git("commit", "-q", "-m", "pr");
  return d;
}
const gate = (d, env = {}) =>
  spawnSync("node", [tool, "gate", "--base", "main"], { cwd: d, encoding: "utf8", env: { ...process.env, PR_BODY: "", ...env } });

test("gate: code without a fragment fails with a message that says what to add", () => {
  const r = gate(gitRepo({ "a.go": "package a\n" }));
  assert.equal(r.status, 1);
  assert.match(r.stdout + r.stderr, /changes\/unreleased\/<issue>-<slug>\.md/);
});

test("gate: an added fragment passes; an invalid one fails", () => {
  assert.equal(gate(gitRepo({ "a.go": "x", "changes/unreleased/1-a.md": frag("Fixed", "x (#1)") })).status, 0);
  assert.equal(gate(gitRepo({ "a.go": "x", "changes/unreleased/1-a.md": frag("Nope", "x") })).status, 1);
});

test("gate: the release commit (new section, fragments gone) passes without a fragment", () => {
  const d = gitRepo({
    "a.go": "x",
    "CHANGELOG.md": HEAD + '## 0.21.0 "Bell Rock" (2026-11-07)\n\n### Fixed\n\n- x\n\n' + OLD,
  });
  assert.equal(gate(d).status, 0);
});

test("gate: a hand edit of an old section is not a release commit", () => {
  const d = gitRepo({ "a.go": "x", "CHANGELOG.md": HEAD + OLD.replace("old fix", "old fix, edited") });
  assert.equal(gate(d).status, 1);
});

test("gate: the PR-body opt-out needs a reason", () => {
  const d = gitRepo({ "a.go": "x" });
  assert.equal(gate(d, { PR_BODY: "changelog: skip - dependency bump" }).status, 0);
  assert.equal(gate(d, { PR_BODY: "changelog: skip" }).status, 1);
  assert.equal(gate(d, { PR_BODY: "<!-- changelog: skip - template hint -->" }).status, 1);
});

// ---- review round (#698) ----
const unchanged = (d) => assert.equal(readFileSync(join(d, "CHANGELOG.md"), "utf8"), HEAD + OLD);

test("CRLF fragments assemble to LF bullets", () => {
  const d = repo({ fragments: { "1-a.md": "---\r\nsection: Fixed\r\n---\r\nwindows line (#1)\r\n" } });
  assert.equal(assemble(d).status, 0);
  const t = readFileSync(join(d, "CHANGELOG.md"), "utf8");
  assert.match(t, /### Fixed\n\n- windows line \(#1\)\n\n/);
  assert.doesNotMatch(t, /\r/);
});

test("a body that starts with a bullet marker is rejected, not doubled", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "- already a bullet") } });
  const r = assemble(d);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /bullet/);
  unchanged(d);
});

test("front matter errors say what is wrong (BOM, extra key, no section)", () => {
  for (const [text, re] of [
    ["﻿" + frag("Fixed", "x"), /byte order mark/],
    ["---\nsection: Fixed\nissue: 4\n---\nx\n", /unknown front matter key "issue"/],
    ["---\n---\nx\n".replace("---\n---", "---\nfoo\n---"), /not "key: value"/],
  ]) {
    const d = repo({ fragments: { "1-a.md": text } });
    const r = assemble(d);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, re);
    unchanged(d);
  }
});

test("a subdirectory or a non-.md file under changes/unreleased/ is an error, not silently ignored", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
  mkdirSync(join(d, "changes/unreleased/sub"));
  writeFileSync(join(d, "changes/unreleased/sub/2-b.md"), frag("Fixed", "lost"));
  let r = assemble(d);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /subdirectories/);
  rmSync(join(d, "changes/unreleased/sub"), { recursive: true });
  writeFileSync(join(d, "changes/unreleased/3-c.txt"), "x");
  r = assemble(d);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /only \.md/);
  unchanged(d);
});

test("an existing section leaves CHANGELOG and fragments untouched", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
  const r = run(d, "assemble", "--version", "0.20.1", "--date", "2026-11-07");
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /already has a section/);
  unchanged(d);
  assert.equal(readdirSync(join(d, "changes/unreleased")).length, 1);
});

test("without any version section yet the new one goes to the end", () => {
  const d = repo({ changelog: "# Changelog\n\nintro\n", fragments: { "1-a.md": frag("Added", "x (#1)") } });
  assert.equal(assemble(d).status, 0);
  assert.equal(readFileSync(join(d, "CHANGELOG.md"), "utf8"), '# Changelog\n\nintro\n\n## 0.21.0 "Bell Rock" (2026-11-07)\n\n### Added\n\n- x (#1)\n');
});

test("DATE must be a real YYYY-MM-DD date, NAME may not contain a quote", () => {
  for (const [args, re] of [
    [["--date", "2026-02-30"], /DATE/],
    [["--date", "tomorrow"], /DATE/],
    [["--date", "2026-1-5"], /DATE/],
    [["--name", 'Bell "Rock"', "--date", "2026-11-07"], /NAME/],
  ]) {
    const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
    const r = run(d, "assemble", "--version", "0.21.0", ...(args[0] === "--name" ? [] : ["--name", "Bell Rock"]), ...args);
    assert.notEqual(r.status, 0, args.join(" "));
    assert.match(r.stderr, re);
    unchanged(d);
  }
});

test("without DATE the heading carries today in Europe/Berlin", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
  const r = run(d, "assemble", "--version", "0.21.0", "--name", "Bell Rock");
  assert.equal(r.status, 0, r.stderr);
  const berlin = new Date().toLocaleDateString("sv-SE", { timeZone: "Europe/Berlin" });
  assert.match(readFileSync(join(d, "CHANGELOG.md"), "utf8"), new RegExp(`^## 0\\.21\\.0 "Bell Rock" \\(${berlin}\\)$`, "m"));
});

test("the heading matches releaseHead of internal/changelog (the What's new parser), version, name and date", () => {
  const go = readFileSync(join(here, "..", "internal/changelog/changelog.go"), "utf8");
  const src = /releaseHead\s*=\s*regexp\.MustCompile\(`([^`]+)`\)/.exec(go)[1];
  const releaseHead = new RegExp(src); // RE2 and JS agree on this expression
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
  assert.equal(assemble(d).status, 0);
  const line = readFileSync(join(d, "CHANGELOG.md"), "utf8").split("\n").find((l) => l.startsWith("## 0.21.0"));
  const m = releaseHead.exec(line);
  assert.deepEqual(m && [m[1], m[2], m[3]], ["0.21.0", "Bell Rock", "2026-11-07"]);
});

test("a version older than the newest section, or a patch of an unknown minor, is rejected", () => {
  for (const v of ["0.19.5", "0.20.0", "0.21.3"]) {
    const d = repo({ fragments: { "1-a.md": frag("Fixed", "x") } });
    const r = run(d, "assemble", "--version", v, "--name", "Tower", "--date", "2026-11-07");
    assert.notEqual(r.status, 0, v);
    assert.match(r.stderr, /older|already has|no 0\.21\.0 section/, v);
    unchanged(d);
  }
});

test("make changelog-assemble takes CL_VERSION (VERSION is the build variable and always set)", () => {
  const d = repo({ fragments: { "1-a.md": frag("Fixed", "x (#1)") } });
  copyMakefile(d);
  mkdirSync(join(d, "scripts"));
  writeFileSync(join(d, "scripts/changelog-assemble.mjs"), readFileSync(tool));
  const make = (...a) => spawnSync("make", ["-s", "changelog-assemble", ...a], { cwd: d, encoding: "utf8" });
  const bad = make("VERSION=0.21.0", "NAME=Bell Rock");
  assert.notEqual(bad.status, 0);
  assert.match(bad.stderr, /CL_VERSION/);
  unchanged(d);
  const ok = make("CL_VERSION=0.21.0", "NAME=Bell Rock", "DATE=2026-11-07");
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(readFileSync(join(d, "CHANGELOG.md"), "utf8"), /^## 0\.21\.0 "Bell Rock" \(2026-11-07\)$/m);
});

const gateCode = (d, code, env = {}) =>
  spawnSync("node", [tool, "gate", "--base", "main", "--code", code], { cwd: d, encoding: "utf8", env: { ...process.env, PR_BODY: "", PR_AUTHOR: "", ...env } });

test("gate: a release commit with fragments left over fails and names them", () => {
  const d = gitRepo({
    "changes/unreleased/1-a.md": frag("Fixed", "left"),
    "CHANGELOG.md": HEAD + '## 0.21.0 "Bell Rock" (2026-11-07)\n\n### Fixed\n\n- x\n\n' + OLD,
  });
  const r = gate(d);
  assert.equal(r.status, 1);
  assert.match(r.stdout, /1-a\.md/);
});

test("gate: a PR that adds '## Unreleased' back is rejected", () => {
  const d = gitRepo({ "CHANGELOG.md": HEAD + "## Unreleased\n\n### Fixed\n\n- x\n\n" + OLD, "changes/unreleased/1-a.md": frag("Fixed", "x") });
  const r = gate(d);
  assert.equal(r.status, 1);
  assert.match(r.stdout, /Unreleased/);
});

test("gate: a docs-only PR still validates fragments (a broken one must not reach main) but needs none", () => {
  assert.equal(gateCode(gitRepo({ "docs/x.md": "x" }), "false").status, 0);
  const bad = gateCode(gitRepo({ "changes/unreleased/1-a.md": frag("Fixes", "x") }), "false");
  assert.equal(bad.status, 1);
  assert.match(bad.stderr, /section/);
});

test("gate: renovate[bot] needs no fragment, anyone else does", () => {
  const d = gitRepo({ "go.mod": "x" });
  assert.equal(gateCode(d, "true", { PR_AUTHOR: "renovate[bot]" }).status, 0);
  assert.equal(gateCode(d, "true", { PR_AUTHOR: "someone" }).status, 1);
});

test("gate: the placeholder <reason> is not a reason", () => {
  const d = gitRepo({ "a.go": "x" });
  const r = gate(d, { PR_BODY: "changelog: skip - <reason>" });
  assert.equal(r.status, 1);
  assert.match(r.stdout, /placeholder/);
});
