// tools/mutation-shard.sh deals the files of the MUTATION packages out to the CI mutation shards by
// estimated cost: runnable mutants (tools/mutation-weights.txt) x seconds per mutant. A file without a
// weights entry used to count as ONE mutant. A file added after the last refresh of the weights is
// exactly that, so a 355-line file (about 85 mutants, minutes of work) was dealt as if it were free: on
// 2026-11-09 shard 4/4 took over the 30 minute job limit because the new files ota/templatejob.go and
// policy/template.go (about 200 mutants together) all landed on the shard that already held the slowest
// package, and the weekly run was cancelled (#729). The estimate for an unweighted file now follows its
// size, so the shards stay balanced between two refreshes of the weights.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

// A plan for internal/cmdpolicy (cmdpolicy.go is its big file, scrub.go and policyrule.go are small) with
// the given weights file content, in two shards. Returns the file names of each shard.
function plan(weights) {
  const dir = mkdtempSync(join(tmpdir(), "mutation-shard-"));
  try {
    const file = join(dir, "weights.txt");
    writeFileSync(file, weights);
    const r = spawnSync("tools/mutation-shard.sh", ["plan", "2"], {
      cwd: root,
      encoding: "utf8",
      env: {
        ...process.env,
        MUTATION_PKGS: "./internal/cmdpolicy",
        MUTATION_WEIGHTS: file,
      },
    });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout
      .trim()
      .split("\n")
      .map((l) => l.split("(estimate):")[1].trim().split(/\s+/));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test("a file without a weights entry is dealt by its size, not as one mutant", () => {
  // cmdpolicy.go has no entry, as a file added after the last refresh. If it counted as one mutant it
  // would share a shard with the small files; it is the biggest file of the package, so it must stand
  // alone on the lighter side of the deal.
  const shards = plan(
    [
      "cost internal/cmdpolicy 27",
      "internal/cmdpolicy scrub.go 24",
      "internal/cmdpolicy policyrule.go 5",
      "internal/cmdpolicy doc.go 0",
      "",
    ].join("\n"),
  );
  const big = shards.find((s) => s.includes("internal/cmdpolicy/cmdpolicy.go"));
  assert.ok(big, "cmdpolicy.go is in a shard");
  assert.deepEqual(big, ["internal/cmdpolicy/cmdpolicy.go"]);
});

test("a file with an entry keeps its measured weight, even when it is small", () => {
  // The estimate by size is only for files nobody measured: an entry of 0 mutants (a file of constants
  // or types) must stay 0, or every doc.go would weigh as much as its lines.
  const shards = plan(
    [
      "cost internal/cmdpolicy 27",
      "internal/cmdpolicy cmdpolicy.go 127",
      "internal/cmdpolicy scrub.go 24",
      "internal/cmdpolicy policyrule.go 5",
      "internal/cmdpolicy doc.go 0",
      "internal/cmdpolicy secret.go 0",
      "",
    ].join("\n"),
  );
  assert.deepEqual(shards[0], ["internal/cmdpolicy/cmdpolicy.go"]);
  assert.deepEqual(shards[1].sort(), [
    "internal/cmdpolicy/doc.go",
    "internal/cmdpolicy/policyrule.go",
    "internal/cmdpolicy/scrub.go",
    "internal/cmdpolicy/secret.go",
  ]);
});

// A toy module with package a (files a1.go, a2.go) and package b (b1.go), so that the deal is shown with
// weights we choose for every file. Returns the plan lines of a two-shard deal as { files, load } per shard.
function toyPlan(weights) {
  const dir = mkdtempSync(join(tmpdir(), "mutation-shard-toy-"));
  try {
    writeFileSync(join(dir, "go.mod"), "module toy\n\ngo 1.22\n");
    for (const [pkg, file] of [["a", "a1"], ["a", "a2"], ["b", "b1"]]) {
      mkdirSync(join(dir, pkg), { recursive: true });
      writeFileSync(join(dir, pkg, `${file}.go`), `package ${pkg}\n\nfunc F${file}() {}\n`);
    }
    writeFileSync(join(dir, "weights.txt"), weights);
    const r = spawnSync(join(root, "tools/mutation-shard.sh"), ["plan", "2"], {
      cwd: dir,
      encoding: "utf8",
      env: { ...process.env, MUTATION_PKGS: "./a ./b", MUTATION_WEIGHTS: join(dir, "weights.txt") },
    });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout
      .trim()
      .split("\n")
      .map((l) => {
        const m = l.match(/load (\d+) s \(estimate\):(.*)$/);
        return { load: Number(m[1]), files: m[2].trim().split(/\s+/).filter(Boolean) };
      });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test("a shard that does not hold a package yet pays the package's setup, so a package stays together", () => {
  // Every shard that mutates a package first builds its tests and runs gremlins' coverage run (about
  // 22 s, #886). A deal that ignores it scatters a package over shards, each of them pays the setup
  // again, and the plan says 17 minutes for a shard that runs 25 (#886). With a 40 s setup, a1.go
  // (100 s) opens shard 1 at 140 s and b1.go (70 s) opens shard 2 at 110 s. The last file, a2.go
  // (60 s), then sees shard 1 at 140 s (it holds package a) and shard 2 at 110 + 40 = 150 s (it would
  // build package a again): it stays with its package, shard 1 ends at 200 s, shard 2 at 110 s.
  // Without a setup line the old deal holds: a2.go goes to the lighter shard 2.
  const cost = ["cost a 100", "cost b 100"];
  const files = ["a a1.go 100", "a a2.go 60", "b b1.go 70"];
  const withSetup = toyPlan([...cost, "setup 4000", ...files, ""].join("\n"));
  assert.deepEqual(withSetup[0].files.sort(), ["a/a1.go", "a/a2.go"]);
  assert.deepEqual(withSetup[1].files, ["b/b1.go"]);
  assert.deepEqual(
    withSetup.map((s) => s.load),
    [200, 110],
  );
  const withoutSetup = toyPlan([...cost, ...files, ""].join("\n"));
  assert.deepEqual(withoutSetup[0].files, ["a/a1.go"]);
  assert.deepEqual(withoutSetup[1].files.sort(), ["a/a2.go", "b/b1.go"]);
});
