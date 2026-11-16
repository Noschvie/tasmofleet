// tools/ha-app-devcontainer-test.sh drives a real Supervisor in a throwaway devcontainer (nightly only). Its step 3
// (backup, uninstall, install, restore) failed once with "Backup does not exist" right after a successful backup
// (#833). That was no race: the script scraped the slug out of the `ha` CLI's YAML output, and the CLI quotes a
// string that YAML would read as a number, so a slug of only digits (03148410, about 2 percent of all slugs) reached
// the restore with literal double quotes. A fake `docker` stands in for the devcontainer, the Supervisor and the
// `ha` CLI, so the tests need no network, no container and no real waiting. The fake CLI mirrors the real one in
// the one way that matters: without --raw-json it prints YAML with the quoting of go.yaml.in/yaml/v3 (the CLI's
// client/yaml.go); with --raw-json it prints the Supervisor's JSON. Its restore, like the Supervisor's
// (supervisor/api/backups.py), only knows the slug that the create call returned.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  existsSync,
  chmodSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const script = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "tools",
  "ha-app-devcontainer-test.sh",
);
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

// Answers every call the script makes; a call it does not know fails loudly and leaves a marker, so a mismatch
// between the fake and the script ends the test instead of hanging in one of the script's retry loops.
const fakeDocker = `#!/usr/bin/env node
const fs = require("fs");
const state = process.env.STATE;
const args = process.argv.slice(2);
const line = args.join(" ");
fs.appendFileSync(state + "/docker.log", line + "\\n");
const read = (n) => fs.readFileSync(state + "/" + n, "utf8").trim();
const ok = (s = "") => { process.stdout.write(s); process.exit(0); };
const bad = (msg, code = 1) => { process.stderr.write(msg + "\\n"); process.exit(code); };

// Would the YAML encoder write this string with quotes? True for what a plain scalar would resolve to as an
// integer or a float (a float that overflows stays a string).
const yamlQuotes = (s) => /^[0-9]+$/.test(s) || (/^[0-9]+e[0-9]+$/.test(s) && Number.isFinite(Number(s)));

function ha(a) {
  const raw = a.includes("--raw-json");
  const [c1, c2] = a;
  if (c1 === "backups" && c2 === "new") {
    const mode = read("create-mode");
    if (mode === "no-slug") return ok(raw ? '{"result":"ok","data":{}}\\n' : "result: ok\\n");
    const slug = read("slug");
    fs.writeFileSync(state + "/created", slug);
    if (raw) return ok(JSON.stringify({ result: "ok", data: { slug } }) + "\\n");
    return ok("slug: " + (yamlQuotes(slug) ? '"' + slug + '"' : slug) + "\\n");
  }
  if (c1 === "backups" && c2 === "restore") {
    if (a[2] !== fs.readFileSync(state + "/created", "utf8")) {
      bad("Error: Backup does not exist");
    }
    fs.writeFileSync(state + "/restored", a[2]);
    return ok();
  }
  if (c1 === "store" && c2 === "apps") return ok("- slug: local_tasmofleet\\n");
  if (c1 === "apps" && c2 === "info") {
    return ok(["name: TasmoFleet", "stage: experimental", "webui: https://[HOST]:8080", "8080/tcp: 8080", "80/tcp: 8081", "options:", "- name: initial_admin_password", ""].join("\\n"));
  }
  if (c1 === "apps" && c2 === "logs") return ok("created the initial admin account from INITIAL_ADMIN_PASSWORD\\n");
  if ((c1 === "store" && c2 === "reload") || (c1 === "apps" && ["install", "uninstall", "start", "stop"].includes(c2))) return ok();
  fs.writeFileSync(state + "/unexpected", line);
  bad("fake docker: unexpected ha " + a.join(" "), 99);
}

const i = args.indexOf("hassio_cli");
if (i >= 0 && args[i + 1] === "ha") ha(args.slice(i + 2));
if (i >= 0 && args[i + 1] === "sh") ok('{"result":"ok"}');
if (args[0] === "exec" && args.includes("sha256sum")) {
  const n = Number(fs.existsSync(state + "/hashes") ? read("hashes") : 0);
  fs.writeFileSync(state + "/hashes", String(n + 1));
  // before, the fresh install, after the restore: the restore brings the original back
  ok(["aaaa", "bbbb", "aaaa"][n] + "  /secret.key\\n");
}
if (args[0] === "exec" && args.includes("curl")) ok("200");
if (args[0] === "rm" || (args[0] === "volume" && args[1] === "rm")) ok();
// everything else the script asks the devcontainer's own Docker: run, info, inspect, tag, push, load, ps, logs
if (["run", "save", "exec"].includes(args[0])) {
  if (args.includes("load")) fs.readFileSync(0);
  ok(args.includes("inspect") ? "ghcr.io/home-assistant/supervisor:test\\n" : "");
}
fs.writeFileSync(state + "/unexpected", line);
bad("fake docker: unexpected call " + line, 99);
`;
const fakeCurl = `#!/bin/sh
exit 0
`;

function run({ slug, createMode = "ok" }) {
  const dir = mkdtempSync(join(tmpdir(), "ha-dev-test-"));
  made.push(dir);
  for (const [n, body] of [
    ["docker", fakeDocker],
    ["curl", fakeCurl],
  ]) {
    writeFileSync(join(dir, n), body);
    chmodSync(join(dir, n), 0o755);
  }
  writeFileSync(join(dir, "slug"), slug);
  writeFileSync(join(dir, "create-mode"), createMode);
  const tmp = join(dir, "tmp");
  spawnSync("mkdir", [tmp]);
  const r = spawnSync("bash", [script, "tasmofleet-ha:test"], {
    encoding: "utf8",
    // The script's retry loops run for minutes when a command keeps failing: fail the test instead of hanging.
    timeout: 30000,
    env: {
      ...process.env,
      PATH: `${dir}:${process.env.PATH}`,
      STATE: dir,
      TMPDIR: tmp,
    },
  });
  const file = (n) =>
    existsSync(join(dir, n)) ? readFileSync(join(dir, n), "utf8") : "";
  return {
    ...r,
    docker: file("docker.log"),
    restored: file("restored"),
    unexpected: file("unexpected"),
  };
}

// Why this matters: one backup in about 40 was named by a slug that the CLI printed with quotes; the nightly then
// went red with no code cause and was re-run until it passed. The slug the restore asks for must be the slug the
// create call returned, whatever that slug looks like.
for (const slug of [
  "a1b2c3d4", // plain
  "03148410", // only digits: the slug of the failed run, printed as "03148410"
  "00000000", // only digits
  "123456e7", // reads as a float, printed with quotes too
  "123e4567", // an overflowing float stays a plain string
]) {
  test(`the restore asks for the slug the backup was created with: ${slug}`, () => {
    const r = run({ slug });
    assert.equal(r.unexpected, "", "the fake met a call it does not know");
    assert.equal(r.status, 0, r.stdout + r.stderr);
    assert.equal(r.restored, slug, "restored the backup that was created");
    assert.match(r.docker, new RegExp(`ha backups restore ${slug} --app `));
    assert.match(r.stdout, /ok: config accepted/);
  });
}

test("a create answer without a slug stops before anything is uninstalled", () => {
  // Without a slug there is nothing to restore: uninstalling first would lose the app's data for nothing. (This
  // guard held before the fix too; the all-digit rows above are what fail without it.)
  const r = run({ slug: "a1b2c3d4", createMode: "no-slug" });
  assert.equal(r.unexpected, "", "the fake met a call it does not know");
  assert.equal(r.status, 1);
  assert.match(r.stderr, /FAIL: no backup slug/);
  assert.doesNotMatch(r.docker, /apps uninstall/);
});
