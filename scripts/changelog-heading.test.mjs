// The release guard and the release title must accept both CHANGELOG heading forms
// ("## X.Y.Z (date)" and "## X.Y.Z "Name" (date)"), so a named release never breaks the tag build.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, copyFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");
const dir = mkdtempSync(join(tmpdir(), "changelog-"));
copyFileSync(join(repo, "Makefile"), join(dir, "Makefile"));
writeFileSync(
  join(dir, "CHANGELOG.md"),
  '## Unreleased\n\n## 0.21.1 "Eddystone" (2026-11-02)\n\npatch\n\n## 0.21.0 "Eddystone" (2026-11-01)\n\nnamed\n\n## 0.20.0 (2026-10-30)\n\nold\n',
);
const make = (target, tag) =>
  execFileSync("make", ["-s", target, `TAG=${tag}`], { cwd: dir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });

test("an unnamed heading passes and yields a plain title", () => {
  make("changelog-check", "v0.20.0");
  assert.equal(make("changelog-title", "v0.20.0"), "TasmoFleet 0.20.0\n");
});

test("a named heading passes, is named in the title and its notes are extracted", () => {
  make("changelog-check", "v0.21.0");
  assert.equal(make("changelog-title", "v0.21.0"), 'TasmoFleet 0.21.0 "Eddystone"\n');
  assert.equal(make("changelog-notes", "v0.21.0").trim(), "named");
});

test("a patch heading repeats its minor's name and the title carries it", () => {
  assert.equal(make("changelog-title", "v0.21.1"), 'TasmoFleet 0.21.1 "Eddystone"\n');
});

test("a missing section still fails the guard", () => {
  assert.throws(() => make("changelog-check", "v0.22.0"));
});
