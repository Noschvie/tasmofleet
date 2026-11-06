// tools/check-agent-refs.py keeps CLAUDE.md and docs/dev honest (#526): an agent that follows a pointer to a
// file, directory, make target or doc that no longer exists wastes its run on a guess. Each test builds a tiny
// repository, breaks exactly one reference and checks that the check names it; the good tree must pass.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const script = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "tools",
  "check-agent-refs.py",
);
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

function repo(claude, extra = {}) {
  const d = mkdtempSync(join(tmpdir(), "agent-refs-"));
  made.push(d);
  const files = {
    Makefile: ".PHONY: check lint\ncheck: lint\n\techo ok\nlint:\n\techo lint\n",
    "CLAUDE.md": claude,
    "tools/real.sh": "#!/bin/sh\n",
    "internal/ota/engine.go": "package ota\n",
    "docs/dev/testing.md": "# Testing\n",
    ...extra,
  };
  for (const [p, c] of Object.entries(files)) {
    mkdirSync(dirname(join(d, p)), { recursive: true });
    writeFileSync(join(d, p), c);
  }
  return d;
}

function run(d) {
  const r = spawnSync("python3", ["-I", script, d], { encoding: "utf8" });
  return { code: r.status, out: r.stdout + r.stderr };
}

test("a guide whose references all exist passes", () => {
  const { code, out } = run(
    repo(
      "Run `make check` (`make lint`), see `tools/real.sh`, `internal/ota/`, `engine.go`, `docs/dev/testing.md`, `internal/ota/eng*.go`.\n" +
        "```\ninternal/ota   state machine\n```\n",
    ),
  );
  assert.equal(code, 0, out);
});

test("a make target that is not in the Makefile fails, naming it", () => {
  const { code, out } = run(repo("Run `make gone-target BASE=x`.\n"));
  assert.equal(code, 1);
  assert.match(out, /make target `gone-target`/);
});

test("a missing file fails, naming it", () => {
  const { code, out } = run(repo("See `tools/renamed.sh`.\n"));
  assert.equal(code, 1);
  assert.match(out, /`tools\/renamed\.sh` does not exist/);
});

test("a missing directory fails, naming it", () => {
  const { code, out } = run(repo("Code lives in `internal/removed/`.\n"));
  assert.equal(code, 1);
  assert.match(out, /internal\/removed/);
});

test("a missing doc fails, also when only docs/dev names it", () => {
  const { code, out } = run(
    repo("fine\n", { "docs/dev/other.md": "Read `docs/pitfalls.md`.\n" }),
  );
  assert.equal(code, 1);
  assert.match(out, /docs\/dev\/other\.md: `docs\/pitfalls\.md`/);
});

test("a package line of a fenced architecture map is checked", () => {
  const { code, out } = run(
    repo("```\ninternal/ghost    does nothing\n```\n"),
  );
  assert.equal(code, 1);
  assert.match(out, /internal\/ghost/);
});

test("placeholders are not references", () => {
  const { code, out } = run(
    repo("Add `NNNN_name.sql`, tag `vX.Y.Z`, file `docs/<topic>.md`.\n"),
  );
  assert.equal(code, 0, out);
});
