import { test, after } from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { Readable } from "node:stream";
import { fileURLToPath } from "node:url";
import {
  PAGES,
  UsageError,
  checkOutDir,
  createOutDir,
  defaultOutDir,
  parseArgs,
  readFirstLine,
  resolvePassword,
  shotName,
} from "../e2e/smoke-shots-lib.mjs";

// The unit tests of the release smoke-check script (#822). The browser part runs in
// e2e/tests/smoke-shots.spec.ts; everything here is pure and needs no network.

const base = ["--url", "https://fleet.example/tasmofleet", "--user", "release"];
const scratch = mkdtempSync(join(tmpdir(), "smoke-shots-test-"));
after(() => rmSync(scratch, { recursive: true, force: true }));

// Why: argv is visible to every local user (ps, /proc) and lands in the shell history. A flag that
// takes the password must not exist, and the refusal must not print the value back.
for (const args of [
  ["--password", "hunter2"],
  ["--password=hunter2"],
  ["--passwd", "hunter2"],
  ["-p", "hunter2"],
  ["--pass=hunter2"],
]) {
  test(`a password in argv is refused and not echoed: ${args[0]}`, () => {
    assert.throws(
      () => parseArgs([...base, ...args]),
      (e) =>
        e instanceof UsageError &&
        /TASMOFLEET_SMOKE_PASSWORD/.test(e.message) &&
        !e.message.includes("hunter2"),
    );
  });
}

// Why: `smoke-shots.mjs ... hunter2` is the other way to put a password into argv; a stray
// positional argument is refused too, without being echoed.
test("a stray positional argument (a password typed after the flags) is refused and not echoed", () => {
  assert.throws(
    () => parseArgs([...base, "hunter2"]),
    (e) => e instanceof UsageError && !e.message.includes("hunter2"),
  );
});

// Why: a URL like https://user:secret@host puts the password into argv just the same.
test("credentials inside the URL are refused and not echoed", () => {
  assert.throws(
    () =>
      parseArgs([
        "--url",
        "https://release:hunter2@fleet.example",
        "--user",
        "release",
      ]),
    (e) => e instanceof UsageError && !e.message.includes("hunter2"),
  );
});

test("an unknown flag, a missing URL or user, and a non-numeric device are refused", () => {
  assert.throws(() => parseArgs([...base, "--frobnicate"]), UsageError);
  assert.throws(() => parseArgs(["--user", "release"]), /--url/);
  assert.throws(() => parseArgs(["--url", "https://fleet.example"]), /--user/);
  assert.throws(() => parseArgs([...base, "--device", "../x"]), /--device/);
  assert.throws(
    () => parseArgs(["--url", "ftp://fleet.example", "--user", "u"]),
    /http/,
  );
});

test("the options are read in both forms and the base path keeps no trailing slash", () => {
  const o = parseArgs([
    "--url=https://fleet.example/tasmofleet/",
    "--user=release",
    "--device",
    "7",
    "--out",
    "/tmp/x",
  ]);
  assert.equal(o.url, "https://fleet.example/tasmofleet");
  assert.equal(o.user, "release");
  assert.equal(o.device, "7");
  assert.equal(o.out, "/tmp/x");
});

// Why: the password comes from the environment or from stdin, in that order; a script that
// signs in with an empty password would only burn one of the few attempts of the login limit.
test("the password comes from the environment first, then from stdin", async () => {
  const never = async () =>
    assert.fail("stdin must not be read when the variable is set");
  assert.equal(
    await resolvePassword({ TASMOFLEET_SMOKE_PASSWORD: "from-env" }, never),
    "from-env",
  );
  assert.equal(
    await resolvePassword({}, async () => "from-stdin"),
    "from-stdin",
  );
  assert.equal(
    await resolvePassword(
      { TASMOFLEET_SMOKE_PASSWORD: "" },
      async () => "from-stdin",
    ),
    "from-stdin",
  );
});

test("an empty password from both sources is refused with the two ways to give it", async () => {
  await assert.rejects(
    resolvePassword({}, async () => ""),
    (e) =>
      e instanceof UsageError &&
      /TASMOFLEET_SMOKE_PASSWORD/.test(e.message) &&
      /stdin/.test(e.message),
  );
});

// Why: `echo secret | script` ends in a newline that is not part of the password; the stream may
// carry more lines, which the script must not wait for (a pipe that stays open would hang).
test("readFirstLine returns the first line without its newline and does not wait for more", async () => {
  assert.equal(
    await readFirstLine(Readable.from(["s3cr", "et\r\nsecond line\n"])),
    "s3cret",
  );
  assert.equal(
    await readFirstLine(Readable.from(["no newline at the end"])),
    "no newline at the end",
  );
  assert.equal(await readFirstLine(Readable.from([])), "");
  const open = new Readable({ read() {} }); // never ends, like a pipe whose writer stays open
  open.push("first\n");
  assert.equal(await readFirstLine(open), "first");
  open.destroy();
});

// Why (ui-review): the screenshots stay outside every repository and are never committed. The
// check must see through symlinks and not be fooled by a path that merely starts with the repo's name.
test("an output path inside the repository is refused: plain, nested, through a symlink, relative", () => {
  const repo = join(scratch, "repo");
  mkdirSync(join(repo, "e2e"), { recursive: true });
  assert.throws(() => checkOutDir(join(repo, "shots"), repo), UsageError);
  assert.throws(
    () => checkOutDir(join(repo, "e2e", "a", "b", "c"), repo),
    UsageError,
  );
  assert.throws(() => checkOutDir(repo, repo), UsageError);
  const link = join(scratch, "link-into-repo");
  symlinkSync(join(repo, "e2e"), link);
  assert.throws(() => checkOutDir(join(link, "shots"), repo), UsageError);
  const outside = join(scratch, "elsewhere");
  mkdirSync(outside);
  symlinkSync(repo, join(outside, "back"));
  assert.throws(
    () => checkOutDir(join(outside, "back", "shots"), repo),
    UsageError,
  );
  const saved = process.cwd();
  process.chdir(join(repo, "e2e"));
  try {
    assert.throws(() => checkOutDir("shots", repo), UsageError);
  } finally {
    process.chdir(saved);
  }
});

test("a sibling whose name starts like the repository is accepted", () => {
  const repo = join(scratch, "tasmofleet");
  mkdirSync(repo);
  assert.doesNotThrow(() =>
    checkOutDir(join(scratch, "tasmofleet-x", "shots"), repo),
  );
  assert.doesNotThrow(() =>
    checkOutDir(join(scratch, "elsewhere-2", "deep", "shots"), repo),
  );
});

// Why: a run never overwrites an earlier one; the evidence of one release must survive the next run.
test("createOutDir makes the directory, accepts an empty one and refuses one that holds files", () => {
  const dir = join(scratch, "runs", "20261116T180000Z");
  createOutDir(dir);
  createOutDir(dir); // still empty: the same run may be started again
  writeFileSync(join(dir, "01-devices.png"), "x");
  assert.throws(
    () => createOutDir(dir),
    (e) => e instanceof UsageError && /already holds/.test(e.message),
  );
});

test("the default output directory sits in the cache, per UTC second, and honours XDG_CACHE_HOME", () => {
  const now = new Date("2026-11-16T18:30:05.123Z");
  assert.equal(
    defaultOutDir({ HOME: "/home/x" }, now),
    "/home/x/.cache/tasmofleet-smoke/20261116T183005Z",
  );
  assert.equal(
    defaultOutDir({ HOME: "/home/x", XDG_CACHE_HOME: "/var/c" }, now),
    "/var/c/tasmofleet-smoke/20261116T183005Z",
  );
  assert.equal(
    defaultOutDir({ HOME: "/home/x", XDG_CACHE_HOME: "relative" }, now),
    "/home/x/.cache/tasmofleet-smoke/20261116T183005Z",
  );
});

test("the shot names are numbered in page order and name the device", () => {
  assert.deepEqual(
    PAGES.map((p, i) => shotName(i, p, "42")),
    [
      "01-devices.png",
      "02-device-42.png",
      "03-transactions.png",
      "04-devices-new.png",
      "05-settings-system.png",
    ],
  );
});

// Why: docs/dev/releasing.md is the list the releaser is held to ("takes a screenshot of each").
// The script keeps its own copy of the list, so a page added to or dropped from the doc (or the
// script) alone must fail here instead of silently shrinking the smoke check.
const releasing = readFileSync(
  resolve(fileURLToPath(import.meta.url), "../../docs/dev/releasing.md"),
  "utf8",
);

/** smokeCheckPages returns the backticked paths of the sub-list in the "Smoke check after the deploy" bullet. */
function smokeCheckPages(md) {
  const start = md.indexOf("- **Smoke check after the deploy**");
  assert.ok(
    start >= 0,
    "releasing.md has no 'Smoke check after the deploy' bullet",
  );
  const rest = md.slice(start + 1);
  const next = rest.search(/\n- \*\*/);
  const bullet = rest.slice(0, next < 0 ? undefined : next);
  return bullet
    .split("\n")
    .filter((l) => /^\s+- /.test(l))
    .map((l) => /`(\/[^`]*)`/.exec(l)?.[1])
    .filter(Boolean);
}

test("the pages of the script are the pages releasing.md lists in its smoke check", () => {
  assert.deepEqual(
    PAGES.map((p) => p.path),
    smokeCheckPages(releasing),
  );
  assert.equal(
    smokeCheckPages(releasing).length,
    5,
    "the doc lists five pages (guards the parser itself)",
  );
});

test("the smoke-check step of releasing.md names the make target and the two password routes", () => {
  const start = releasing.indexOf("- **Smoke check after the deploy**");
  const bullet = releasing.slice(start, releasing.indexOf("\n- **", start + 1));
  assert.match(bullet, /make smoke-shots/);
  assert.match(bullet, /TASMOFLEET_SMOKE_PASSWORD/);
  assert.match(bullet, /stdin/);
});
