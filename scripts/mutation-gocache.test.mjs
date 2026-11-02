// gremlins builds the package and its test binary once per mutant, and every one of those builds lands
// in the Go build cache (154 mutants of internal/cmdpolicy: +261 MB; parallel runs filled a 255 GB disk,
// #546). A mutant's build is never used again, so tools/mutation-gate.sh runs gremlins with a throwaway
// GOCACHE that is gone when the gate ends. These tests run the real gate with a stub tool, a stub go and
// a stub gremlins that writes into the GOCACHE it is given, like a mutant build does, and check that the
// caller's GOCACHE stays as it was and that nothing is left behind, whatever way the run ends.
//
// gremlins times its first (coverage) run and gives every mutant a multiple of it as its timeout, so that
// run must not pay for a cold cache (ota: 11 s cold, 2.6 s warm, a hanging mutant would block a worker for
// minutes): the gate warms the throwaway cache with one non-mutant build of the package before gremlins
// starts. The stub gremlins checks that the warm-up's build is in its cache.
import { test, after } from "node:test";
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
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const gate = join(
  dirname(fileURLToPath(import.meta.url)),
  "../tools/mutation-gate.sh",
);

const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

function setup() {
  const dir = mkdtempSync(join(tmpdir(), "mutation-gocache-"));
  made.push(dir);
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
  // The go the Makefile selected: its GOROOT, the real cache, and the warm-up build (a non-mutant build:
  // it lands in the GOCACHE it is given, which must be the throwaway one).
  exe(
    "go",
    `case "$*" in
"env GOROOT") echo "${dir}/goroot" ;;
"env GOCACHE") echo "$GOCACHE" ;;
test*)
	echo "$*" > "${dir}/warm-args"
	echo "$GOCACHE" > "${dir}/warm-cache"
	[ "$STUB_MODE" = warmfail ] && { echo "build failed" >&2; exit 1; }
	mkdir -p "$GOCACHE/cd" && echo dependency > "$GOCACHE/cd/0456-d" ;;
*) echo "stub go: unexpected arguments: $*" >&2; exit 2 ;;
esac`,
  );
  // A mutant build: a new file in whatever GOCACHE the run was given.
  exe(
    "gremlins",
    `echo "$GOCACHE" > "${dir}/seen"
echo "\${PATH%%:*}" > "${dir}/path-first"
[ -d "$GOCACHE" ] && echo yes > "${dir}/existed"
echo "$STUB_NICED" > "${dir}/niced"
[ -f "$GOCACHE/cd/0456-d" ] && echo yes > "${dir}/warmed"
[ -f "$GOCACHE/00/dep-d" ] && echo yes > "${dir}/seeded"
mkdir -p "$GOCACHE/ab" && echo mutant > "$GOCACHE/ab/0123-d"
case "$STUB_MODE" in
fail) echo "boom" >&2; exit 1 ;;
hang) touch "${dir}/started"; sleep 60 ;;
esac
echo "Killed: 8, Lived: 0, Not covered: 0 Timed out: 0, Not viable: 0, Skipped: 0"`,
  );
  // The real cache sits in a directory of its own, like ~/.cache/go-build: the throwaway one goes next to it.
  const home = join(dir, "home");
  const shared = join(home, "go-build");
  mkdirSync(join(shared, "00"), { recursive: true });
  writeFileSync(join(shared, "00", "dep-d"), "a dependency build\n");
  const tmp = join(dir, "tmp");
  mkdirSync(tmp);
  const env = (extra = {}) => ({
    ...process.env,
    GO: join(dir, "go"),
    TOOL: join(dir, "tool"),
    NICE: "env STUB_NICED=yes",
    GOCACHE: shared,
    TMPDIR: tmp,
    MUTATION_WORKERS: "2",
    ...extra,
  });
  const files = (d) => readdirSync(d, { recursive: true }).sort();
  const read = (f) => readFileSync(join(dir, f), "utf8").trim();
  const runGate = (extra) =>
    spawnSync("sh", [gate, "./internal/p", "50"], {
      cwd: dir,
      env: env(extra),
      encoding: "utf8",
    });
  // Nothing of the run is left: only the real cache next to where the throwaway one was, nothing in TMPDIR,
  // the real cache itself as it was.
  const assertClean = () => {
    assert.deepEqual(
      readdirSync(home),
      ["go-build"],
      "the throwaway GOCACHE was not removed",
    );
    assert.deepEqual(readdirSync(tmp), [], "the gate left files in TMPDIR");
    assert.deepEqual(
      files(shared),
      ["00", "00/dep-d"],
      "a mutant build reached the shared GOCACHE",
    );
  };
  return { dir, home, shared, tmp, env, files, read, runGate, assertClean };
}

test("mutant builds go to a throwaway GOCACHE, the caller's one stays untouched and nothing is left", () => {
  const s = setup();
  const r = s.runGate();
  assert.equal(r.status, 0, r.stdout + r.stderr);
  s.assertClean();
  const seen = s.read("seen");
  assert.notEqual(seen, s.shared, "gremlins ran with the caller's GOCACHE");
  // on disk next to the real cache, not in TMPDIR (a tmpfs: a whole ota run puts about 1 GB there)
  assert.equal(
    dirname(seen),
    s.home,
    `the throwaway cache is not next to the real one: ${seen}`,
  );
  assert.equal(s.read("existed"), "yes");
  // gremlins itself is built through the real cache (cold in a throwaway one it costs seconds per run)
  assert.equal(s.read("tool-cache"), s.shared);
  // the Makefile's nice prefix still wraps the gremlins run
  assert.equal(s.read("niced"), "yes");
});

test("the throwaway GOCACHE is warmed with a non-mutant build before gremlins starts, so its coverage run (the base of every mutant's timeout) is not a cold one", () => {
  const s = setup();
  const r = s.runGate();
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.equal(
    s.read("warm-cache"),
    s.read("seen"),
    "the warm-up built into another cache than gremlins uses",
  );
  assert.equal(s.read("warm-args"), "test -cover -run ^$ ./internal/p/...");
  assert.equal(
    s.read("warmed"),
    "yes",
    "gremlins started before the warm-up's build was in its cache",
  );
});

test("gremlins runs with the go that GO selected first on PATH, not whichever go comes first", () => {
  const s = setup();
  const r = s.runGate();
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.equal(s.read("path-first"), join(s.dir, "goroot", "bin"));
});

test("a package that does not build fails the gate before gremlins starts, and leaves nothing", () => {
  const s = setup();
  const r = s.runGate({ STUB_MODE: "warmfail" });
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stdout + r.stderr, /could not build \.\/internal\/p/);
  assert.ok(
    !existsSync(join(s.dir, "seen")),
    "gremlins ran although the package does not build",
  );
  s.assertClean();
});

test("the throwaway GOCACHE is removed when gremlins fails", () => {
  const s = setup();
  const r = s.runGate({ STUB_MODE: "fail" });
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stdout + r.stderr, /gremlins failed/);
  assert.ok(
    existsSync(join(s.dir, "seen")),
    "gremlins never ran, so this tested nothing",
  );
  s.assertClean();
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
    readdirSync(s.home).length,
    2,
    "expected the real cache and the throwaway one while running",
  );
  process.kill(-child.pid, "SIGINT");
  await exited;
  s.assertClean();
});

test("MUTATION_GOCACHE_SEED warms the throwaway GOCACHE and is never written to", () => {
  const s = setup();
  assert.equal(s.runGate().status, 0);
  assert.ok(
    !existsSync(join(s.dir, "seeded")),
    "the dependency build was there without a seed",
  );
  const r = s.runGate({ MUTATION_GOCACHE_SEED: s.shared });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.ok(
    existsSync(join(s.dir, "seeded")),
    "gremlins did not find the seed's builds",
  );
  s.assertClean();
});
