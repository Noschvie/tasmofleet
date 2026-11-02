// gremlins builds the package and its test binary once per mutant, and every one of those builds lands
// in the Go build cache (154 mutants of internal/cmdpolicy: +261 MB; parallel runs filled a 255 GB disk,
// #546). A mutant's build is never used again, so tools/mutation-gate.sh runs gremlins with a throwaway
// GOCACHE that is gone when the gate ends. These tests run the real gate with a stub tool and a stub
// gremlins that writes into the GOCACHE it is given, like a mutant build does, and check that the
// caller's GOCACHE stays as it was and that nothing is left behind, whatever way the run ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  readdirSync,
  existsSync,
  chmodSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const gate = join(
  dirname(fileURLToPath(import.meta.url)),
  "../tools/mutation-gate.sh",
);

function setup() {
  const dir = mkdtempSync(join(tmpdir(), "mutation-gocache-"));
  const exe = (name, body) => {
    writeFileSync(join(dir, name), `#!/bin/sh\n${body}\n`);
    chmodSync(join(dir, name), 0o755);
  };
  // The tool is `go tool -modfile=...`: `-n gremlins` prints the path of the gremlins binary, `gremlins ...`
  // runs it with the caller's environment.
  exe(
    "tool",
    `case "$*" in
"-n gremlins") echo "$GOCACHE" > "${dir}/tool-cache"; echo "${dir}/gremlins" ;;
"gremlins "*) shift; exec "${dir}/gremlins" "$@" ;;
*) echo "stub tool: unexpected arguments: $*" >&2; exit 2 ;;
esac`,
  );
  // A mutant build: a new file in whatever GOCACHE the run was given.
  exe(
    "gremlins",
    `echo "$GOCACHE" > "${dir}/seen"
[ -d "$GOCACHE" ] && echo yes > "${dir}/existed"
echo "$STUB_NICED" > "${dir}/niced"
[ -f "$GOCACHE/00/dep-d" ] && echo yes > "${dir}/seeded"
mkdir -p "$GOCACHE/ab" && echo mutant > "$GOCACHE/ab/0123-d"
case "$STUB_MODE" in
fail) echo "boom" >&2; exit 1 ;;
hang) touch "${dir}/started"; sleep 60 ;;
esac
echo "Killed: 8, Lived: 0, Not covered: 0 Timed out: 0, Not viable: 0, Skipped: 0"`,
  );
  const shared = join(dir, "shared-gocache");
  mkdirSync(join(shared, "00"), { recursive: true });
  writeFileSync(join(shared, "00", "dep-d"), "a dependency build\n");
  const tmp = join(dir, "tmp");
  mkdirSync(tmp);
  const env = (extra = {}) => ({
    ...process.env,
    TOOL: join(dir, "tool"),
    NICE: "env STUB_NICED=yes",
    GOCACHE: shared,
    TMPDIR: tmp,
    MUTATION_WORKERS: "2",
    ...extra,
  });
  const files = (d) => readdirSync(d, { recursive: true }).sort();
  return { dir, shared, tmp, env, files };
}

test("mutant builds go to a throwaway GOCACHE, the caller's one stays untouched and nothing is left", () => {
  const s = setup();
  const before = s.files(s.shared);
  const r = spawnSync("sh", [gate, "./internal/p", "50"], {
    cwd: s.dir,
    env: s.env(),
    encoding: "utf8",
  });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.deepEqual(
    s.files(s.shared),
    before,
    "a mutant build reached the shared GOCACHE",
  );
  const seen = readFileSync(join(s.dir, "seen"), "utf8").trim();
  assert.notEqual(seen, s.shared, "gremlins ran with the caller's GOCACHE");
  assert.ok(
    seen.startsWith(s.tmp),
    `the throwaway cache is not under TMPDIR: ${seen}`,
  );
  assert.equal(readFileSync(join(s.dir, "existed"), "utf8").trim(), "yes");
  assert.deepEqual(readdirSync(s.tmp), [], "the gate left files in TMPDIR");
  // gremlins itself is built through the real cache (cold in a throwaway one it costs seconds per run)
  assert.equal(
    readFileSync(join(s.dir, "tool-cache"), "utf8").trim(),
    s.shared,
  );
  // the Makefile's nice prefix still wraps the gremlins run
  assert.equal(readFileSync(join(s.dir, "niced"), "utf8").trim(), "yes");
});

test("the throwaway GOCACHE is removed when gremlins fails", () => {
  const s = setup();
  const r = spawnSync("sh", [gate, "./internal/p", "50"], {
    cwd: s.dir,
    env: s.env({ STUB_MODE: "fail" }),
    encoding: "utf8",
  });
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stdout + r.stderr, /gremlins failed/);
  assert.ok(existsSync(join(s.dir, "seen")));
  assert.deepEqual(readdirSync(s.tmp), [], "the gate left files in TMPDIR");
  assert.deepEqual(s.files(s.shared), ["00", "00/dep-d"]);
});

test("the throwaway GOCACHE is removed on Ctrl-C", async () => {
  const s = setup();
  // Own process group, so the signal reaches the gate and gremlins like a terminal's Ctrl-C does.
  const child = spawn("sh", [gate, "./internal/p", "50"], {
    cwd: s.dir,
    env: s.env({ STUB_MODE: "hang" }),
    stdio: "ignore",
    detached: true,
  });
  const exited = new Promise((resolve) =>
    child.on("exit", (code, sig) => resolve({ code, sig })),
  );
  for (let i = 0; i < 100 && !existsSync(join(s.dir, "started")); i++) {
    await new Promise((r) => setTimeout(r, 50));
  }
  assert.ok(existsSync(join(s.dir, "started")), "gremlins never started");
  assert.equal(
    readdirSync(s.tmp).length,
    2,
    "expected the output file and the cache while running",
  );
  process.kill(-child.pid, "SIGINT");
  await exited;
  assert.deepEqual(readdirSync(s.tmp), [], "the gate left files in TMPDIR");
  assert.deepEqual(s.files(s.shared), ["00", "00/dep-d"]);
});

test("MUTATION_GOCACHE_SEED warms the throwaway GOCACHE and is never written to", () => {
  const s = setup();
  const run = (extra) =>
    spawnSync("sh", [gate, "./internal/p", "50"], {
      cwd: s.dir,
      env: s.env(extra),
      encoding: "utf8",
    });
  assert.equal(run({}).status, 0);
  assert.ok(
    !existsSync(join(s.dir, "seeded")),
    "the dependency build was there without a seed",
  );
  const r = run({ MUTATION_GOCACHE_SEED: s.shared });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.ok(
    existsSync(join(s.dir, "seeded")),
    "gremlins did not find the seed's builds",
  );
  assert.deepEqual(s.files(s.shared), ["00", "00/dep-d"]);
  assert.deepEqual(readdirSync(s.tmp), []);
});
