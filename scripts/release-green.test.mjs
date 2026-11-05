// tools/release-green.sh decides whether a tag may publish without running the checks again (#554).
// A fake `gh` answers from fixtures, so no network is needed. Each test names a way a commit can be
// not green and checks that the release is refused for exactly that reason.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync, chmodSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const script = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "tools",
  "release-green.sh",
);
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

// The fake gh: `gh api [--paginate] ENDPOINT [--jq EXPR]` prints FIXTURES[endpoint prefix], through jq when --jq is given.
const fakeGh = `#!/bin/sh
shift
ep=; jq=
while [ $# -gt 0 ]; do
  case "$1" in
    --paginate) ;;
    --jq) shift; jq=$1 ;;
    *) ep=$1 ;;
  esac
  shift
done
case "$ep" in
  */check-runs\\?*) f=checks ;;
  */actions/runs\\?*) f=runs ;;
  */actions/runs/*/jobs*) f=jobs ;;
  */actions/runs/*) f=run ;;
  *) echo "unexpected $ep" >&2; exit 1 ;;
esac
if [ -n "$jq" ]; then jq -r "$jq" "$FIXTURES/$f.json"; else cat "$FIXTURES/$f.json"; fi
`;

function run(fx) {
  const dir = mkdtempSync(join(tmpdir(), "release-green-"));
  made.push(dir);
  writeFileSync(join(dir, "gh"), fakeGh);
  chmodSync(join(dir, "gh"), 0o755);
  const d = {
    checks: {
      check_runs: [
        {
          started_at: "2026-11-05T10:00:00Z",
          status: "completed",
          conclusion: "success",
          html_url: "https://github.com/o/r/actions/runs/7/job/1",
        },
      ],
    },
    run: { path: ".github/workflows/pr.yml" },
    runs: {
      workflow_runs: [
        {
          id: 9,
          path: ".github/workflows/main.yml",
          status: "completed",
          created_at: "2026-11-05T10:10:00Z",
        },
      ],
    },
    jobs: {
      jobs: [
        { name: "checks / lint", conclusion: "success" },
        { name: "image / build image (edge)", conclusion: "success" },
        { name: "ghcr-cleanup / prune", conclusion: "success" },
      ],
    },
    ...fx,
  };
  for (const [k, v] of Object.entries(d))
    writeFileSync(join(dir, `${k}.json`), JSON.stringify(v));
  return spawnSync("bash", [script, "abc123"], {
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: `${dir}:${process.env.PATH}`,
      FIXTURES: dir,
      GITHUB_REPOSITORY: "o/r",
    },
  });
}

test("a commit with a green required and a green main.yml run is accepted", () => {
  const r = run({});
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /abc123 is green/);
});

test("a commit without a required check run is refused", () => {
  const r = run({ checks: { check_runs: [] } });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /never green as a pull request/);
});

test("a failed required is refused", () => {
  const r = run({
    checks: {
      check_runs: [
        {
          started_at: "2026-11-05T10:00:00Z",
          status: "completed",
          conclusion: "failure",
          html_url: "https://github.com/o/r/actions/runs/7/job/1",
        },
      ],
    },
  });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /concluded failure/);
});

test('a job called "required" of another workflow does not count', () => {
  const r = run({ run: { path: ".github/workflows/other.yml" } });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /never green as a pull request/);
});

test("a commit without a main.yml run is refused", () => {
  const r = run({ runs: { workflow_runs: [] } });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /no main\.yml run/);
});

test("a main.yml run that is still running is refused", () => {
  const r = run({
    runs: {
      workflow_runs: [
        {
          id: 9,
          path: ".github/workflows/main.yml",
          status: "in_progress",
          created_at: "2026-11-05T10:10:00Z",
        },
      ],
    },
  });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /not finished/);
});

test("a failed job of main.yml is refused, but ghcr-cleanup may fail", () => {
  const bad = run({
    jobs: { jobs: [{ name: "checks / lint", conclusion: "failure" }] },
  });
  assert.notEqual(bad.status, 0);
  assert.match(bad.stderr, /checks \/ lint: failure/);
  const ok = run({
    jobs: {
      jobs: [
        { name: "checks / lint", conclusion: "success" },
        { name: "ghcr-cleanup / prune", conclusion: "failure" },
      ],
    },
  });
  assert.equal(ok.status, 0, ok.stderr);
});
