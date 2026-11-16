// tools/dist.sh (make dist, #472) builds the native release archives that release.yml attaches to the GitHub
// release next to the image. A fake `go` records how it was called and writes that record as the "binary", so
// no toolchain run and no network are needed. Each test names what a user of the downloaded archive relies on.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  readdirSync,
  chmodSync,
  statSync,
  rmSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repo = join(dirname(fileURLToPath(import.meta.url)), "..");
const script = join(repo, "tools", "dist.sh");
const made = [];
after(() => {
  for (const d of made) rmSync(d, { recursive: true, force: true });
});

function tmp(prefix) {
  const d = mkdtempSync(join(tmpdir(), prefix));
  made.push(d);
  return d;
}

// The fake go: `go build [flags] -o OUT PKG` writes its environment and arguments to OUT. It fails for the
// architecture named in FAIL_GOARCH, as a broken cross-compile would.
const fakeGo = `#!/bin/sh
[ "$1" = build ] || { echo "fake go: unexpected $*" >&2; exit 2; }
[ "$GOARCH" != "\${FAIL_GOARCH:-none}" ] || { echo "fake go: build failed for $GOARCH" >&2; exit 1; }
out=
prev=
for a in "$@"; do
  [ "$prev" = -o ] && out=$a
  prev=$a
done
[ -n "$out" ] || { echo "fake go: no -o" >&2; exit 2; }
printf 'GOOS=%s\\nGOARCH=%s\\nCGO_ENABLED=%s\\nARGS=%s\\n' "$GOOS" "$GOARCH" "\${CGO_ENABLED-unset}" "$*" > "$out"
`;

function fake() {
  const dir = tmp("dist-go-");
  const go = join(dir, "go");
  writeFileSync(go, fakeGo);
  chmodSync(go, 0o755);
  return go;
}

// A clean environment: CGO_ENABLED=1 as on a host with a C compiler, so only the script can switch it off, and
// no MAKEFLAGS of an outer make (make scripts-test) leaking into the make this test starts.
function env(extra) {
  const e = { ...process.env, CGO_ENABLED: "1", ...extra };
  for (const k of [
    "MAKEFLAGS",
    "MFLAGS",
    "MAKELEVEL",
    "VERSION",
    "LDFLAGS",
    "BUILDFLAGS",
    "DIST",
    "GO",
  ]) {
    if (!(k in extra)) delete e[k];
  }
  return e;
}

// Runs the script directly, the way make dist does, with the Makefile's flags for VERSION.
function dist({ version = "1.2.3", ldflags, failArch, out } = {}) {
  out ??= tmp("dist-out-");
  const r = spawnSync("bash", [script], {
    cwd: repo,
    encoding: "utf8",
    env: env({
      GO: fake(),
      VERSION: version,
      LDFLAGS: ldflags ?? `-s -w -X main.version=${version}`,
      BUILDFLAGS: "-trimpath",
      DIST: out,
      ...(failArch ? { FAIL_GOARCH: failArch } : {}),
    }),
  });
  return { ...r, out };
}

function listTar(archive) {
  const r = spawnSync("tar", ["-tvzf", archive], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return r.stdout.trim().split("\n");
}

function extract(archive) {
  const d = tmp("dist-x-");
  const r = spawnSync("tar", ["-xzf", archive, "-C", d], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return d;
}

// Why: a Raspberry Pi user must get an aarch64 binary and a PC user an x86-64 one; a CGO binary would need the
// build host's libc and not run on every distribution; the About page and bug reports name the version stamped
// by -X main.version, so it must be the release's. The AGPL requires the licence text in every copy, and the
// archive carries the unit the install guide uses. Run through make, so the Makefile's flags (LDFLAGS with the
// version, BUILDFLAGS with -trimpath) are the ones that reach go build.
test("make dist builds one static archive per architecture with the release's version, the licence and the unit", () => {
  const out = tmp("dist-out-");
  const r = spawnSync(
    "make",
    [
      "-s",
      "--no-print-directory",
      "dist",
      "VERSION=1.2.3",
      `GO=${fake()}`,
      `DIST=${out}`,
      "NICE=",
    ],
    {
      cwd: repo,
      encoding: "utf8",
      env: env({}),
    },
  );
  assert.equal(r.status, 0, r.stderr);
  for (const arch of ["amd64", "arm64"]) {
    const name = `tasmofleet_1.2.3_linux_${arch}`;
    const archive = join(out, `${name}.tar.gz`);
    assert.ok(
      existsSync(archive),
      `${name}.tar.gz missing in ${readdirSync(out)}`,
    );
    const entries = listTar(archive).map((l) => l.split(/\s+/).pop());
    assert.deepEqual(entries.sort(), [
      `${name}/`,
      `${name}/LICENSE`,
      `${name}/README.md`,
      `${name}/tasmofleet`,
      `${name}/tasmofleet.service`,
    ]);
    const x = join(extract(archive), name);
    const bin = readFileSync(join(x, "tasmofleet"), "utf8");
    assert.match(bin, /^GOOS=linux$/m);
    assert.match(bin, new RegExp(`^GOARCH=${arch}$`, "m"));
    assert.match(
      bin,
      /^CGO_ENABLED=0$/m,
      "the binary must be static (CGO off)",
    );
    assert.match(bin, /-X main\.version=1\.2\.3( |$)/m);
    assert.match(bin, /-trimpath/);
    assert.match(bin, /\.\/cmd\/tasmofleet$/m);
    assert.equal(
      statSync(join(x, "tasmofleet")).mode & 0o777,
      0o755,
      "the binary must be executable",
    );
    assert.equal(
      readFileSync(join(x, "LICENSE"), "utf8"),
      readFileSync(join(repo, "LICENSE"), "utf8"),
    );
    assert.equal(
      readFileSync(join(x, "tasmofleet.service"), "utf8"),
      readFileSync(
        join(repo, "deploy", "systemd", "tasmofleet.service"),
        "utf8",
      ),
    );
  }
});

// Why: users check the download against SHA256SUMS before they run it as a service; it must verify and name
// exactly the archives of the release, nothing missing and nothing else.
test("SHA256SUMS verifies and lists exactly the archives", () => {
  const r = dist();
  assert.equal(r.status, 0, r.stderr);
  const check = spawnSync("sha256sum", ["-c", "SHA256SUMS"], {
    cwd: r.out,
    encoding: "utf8",
  });
  assert.equal(check.status, 0, check.stdout + check.stderr);
  const listed = readFileSync(join(r.out, "SHA256SUMS"), "utf8")
    .trim()
    .split("\n")
    .map((l) => l.split(/\s+\*?/).pop())
    .sort();
  const archives = readdirSync(r.out)
    .filter((f) => f.endsWith(".tar.gz"))
    .sort();
  assert.deepEqual(listed, [
    "tasmofleet_1.2.3_linux_amd64.tar.gz",
    "tasmofleet_1.2.3_linux_arm64.tar.gz",
  ]);
  assert.deepEqual(archives, listed);
  assert.deepEqual(readdirSync(r.out).sort(), [...listed, "SHA256SUMS"].sort());
});

// Why: the asset names and the version the binary reports must match the image tag (X.Y.Z, no v). A dev build
// (the Makefile's default VERSION is git describe, with a v) must not be packaged as a release by mistake.
for (const version of ["", "dev", "v1.2.3", "1.2", "1.2.3+dirty"]) {
  test(`VERSION '${version}' is refused and nothing is written`, () => {
    const r = dist({ version });
    assert.notEqual(r.status, 0, `accepted VERSION '${version}'`);
    // Refused for this reason, not because something else broke.
    assert.match(r.stderr, /VERSION must be X\.Y\.Z/);
    assert.deepEqual(readdirSync(r.out), []);
  });
}

// Why: the archive name says 1.2.3, so the binary inside must report 1.2.3 too; LDFLAGS stamped for another
// version (a stale make variable) would ship a binary that names the wrong release in bug reports.
test("LDFLAGS that stamp another version are refused", () => {
  const r = dist({ version: "1.2.3", ldflags: "-s -w -X main.version=1.2.4" });
  assert.notEqual(r.status, 0, "packaged 1.2.3 with a binary stamped 1.2.4");
  assert.match(r.stderr, /do not stamp main\.version=1\.2\.3/);
  assert.deepEqual(readdirSync(r.out), []);
});

// Why: a half set must never be released. When one architecture fails to build, dist fails, writes no
// archive and no SHA256SUMS, and removes a stale SHA256SUMS of an earlier run, so nothing can be attached.
test("a failed arm64 build fails dist and leaves no archive and no SHA256SUMS", () => {
  const out = tmp("dist-out-");
  writeFileSync(join(out, "SHA256SUMS"), "stale\n");
  const r = dist({ failArch: "arm64", out });
  assert.notEqual(
    r.status,
    0,
    "dist succeeded although the arm64 build failed",
  );
  assert.match(r.stderr, /build failed for arm64/);
  assert.deepEqual(readdirSync(out), []);
});
