// scripts/pages-check.mjs keeps the website's pictures build output (#859). The project page showed screenshots of
// 0.28.0 next to a demo of 0.37.0 because the pictures were hand-made copies in site/assets. The first test pins that
// the repository holds no such copy (it fails as long as one is committed); the others pin that a Pages artifact
// without the generated pictures is refused, so a failed demo-media run can never publish a page with broken images.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const script = join(root, "scripts", "pages-check.mjs");
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

function tree(files) {
  const d = mkdtempSync(join(tmpdir(), "pages-check-"));
  made.push(d);
  for (const [p, c] of Object.entries(files)) {
    mkdirSync(dirname(join(d, p)), { recursive: true });
    writeFileSync(join(d, p), c);
  }
  return d;
}

const run = (...args) => spawnSync("node", [script, ...args], { encoding: "utf8" });

const page = `<link rel="icon" href="assets/favicon.svg"><link rel="stylesheet" href="style.css">
<a href="demo/">Demo</a><a href="#install">Install</a><a href="https://github.com/hellwand/tasmofleet">GitHub</a>
<img src="assets/dashboard.png"><img src="assets/demo.webp?v=1">`;
const complete = {
  "index.html": page,
  "style.css": "body{}",
  "demo/index.html": "<html></html>",
  "assets/favicon.svg": "<svg/>",
  "assets/dashboard.png": "png",
  "assets/demo.webp": "webp",
};

test("the repository's website holds no committed picture", () => {
  const r = run("--source", join(root, "site"));
  assert.equal(r.status, 0, r.stderr);
});

test("a committed screenshot in the website source is refused", () => {
  const d = tree({ "index.html": page, "assets/favicon.svg": "<svg/>", "assets/dashboard.png": "png" });
  const r = run("--source", d);
  assert.equal(r.status, 1);
  assert.match(r.stderr, /assets\/dashboard\.png: a committed picture/);
});

test("fonts, wordmarks and the favicon are source files, not pictures", () => {
  const d = tree({ "assets/geist-latin.woff2": "f", "assets/wordmark.svg": "<svg/>", "assets/favicon.svg": "<svg/>" });
  assert.equal(run("--source", d).status, 0);
});

test("a complete Pages artifact passes (outside links, anchors and query strings are not files)", () => {
  assert.equal(run(tree(complete)).status, 0);
});

test("an artifact without the generated screenshots is refused", () => {
  const { ["assets/dashboard.png"]: _gone, ...rest } = complete;
  const r = run(tree(rest));
  assert.equal(r.status, 1);
  assert.match(r.stderr, /links assets\/dashboard\.png \(missing\)/);
});

test("an empty animation is refused (a failed conversion must not publish)", () => {
  const r = run(tree({ ...complete, "assets/demo.webp": "" }));
  assert.equal(r.status, 1);
  assert.match(r.stderr, /links assets\/demo\.webp \(empty\)/);
});
