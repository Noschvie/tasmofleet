// The pure parts of smoke-shots.mjs (#822): arguments, the password source, the output directory and the
// page list. No dependencies, so scripts/smoke-shots.test.mjs tests them with plain node in CI.
import { existsSync, mkdirSync, readdirSync, realpathSync } from "node:fs";
import { dirname, isAbsolute, join, resolve, sep } from "node:path";
import { homedir } from "node:os";

/** UsageError is a refusal the person can fix; the message never contains a password or an argument's value. */
export class UsageError extends Error {}

/**
 * PAGES are the pages docs/dev/releasing.md lists in its smoke check, in that order; scripts/smoke-shots.test.mjs
 * keeps the two lists equal. "<id>" is the device that is online.
 */
export const PAGES = [
  { name: "devices", path: "/devices" },
  { name: "device", path: "/devices/<id>" },
  { name: "transactions", path: "/transactions" },
  { name: "devices-new", path: "/devices/new" },
  { name: "settings-system", path: "/settings/system" },
];

/** VIEWPORT is the owner's viewport (docs/dev/ui-review.md). */
export const VIEWPORT = { width: 1280, height: 800 };

export const PASSWORD_HELP =
  "the password is never an argument (ps and the shell history show it): set TASMOFLEET_SMOKE_PASSWORD or pipe it on stdin";

const VALUE_FLAGS = new Set(["--url", "--user", "--device", "--out"]);
// anything that looks like it carries a secret; the whole flag is refused, its value is never looked at
const SECRET_FLAG = /^-{1,2}(p|pw|pass\w*|secret\w*|token\w*)$/i;

/** parseArgs reads --url, --user, --device, --out (also as --flag=value) and --help; anything else is a UsageError. */
export function parseArgs(argv) {
  const o = { url: "", user: "", device: "", out: "", help: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--help" || arg === "-h") {
      o.help = true;
      continue;
    }
    if (!arg.startsWith("-")) throw new UsageError(`unexpected argument (the script takes only --url, --user, --device, --out); ${PASSWORD_HELP}`);
    const eq = arg.indexOf("=");
    const flag = eq < 0 ? arg : arg.slice(0, eq);
    if (SECRET_FLAG.test(flag)) throw new UsageError(`${flag} is refused: ${PASSWORD_HELP}`);
    if (!VALUE_FLAGS.has(flag)) throw new UsageError(`unknown option ${flag}`);
    let value;
    if (eq >= 0) value = arg.slice(eq + 1);
    else {
      value = argv[++i];
      if (value === undefined) throw new UsageError(`${flag} needs a value`);
    }
    o[flag.slice(2)] = value;
  }
  if (o.help) return o;
  if (!o.url) throw new UsageError("--url is required (the address of the instance, with its base path)");
  if (!o.user) throw new UsageError("--user is required");
  let u;
  try {
    u = new URL(o.url);
  } catch {
    throw new UsageError("--url is not a URL");
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") throw new UsageError("--url must be an http or https address");
  if (u.username || u.password) throw new UsageError(`--url must not carry credentials: ${PASSWORD_HELP}`);
  o.url = o.url.replace(/\/+$/, "");
  if (o.device && !/^\d+$/.test(o.device)) throw new UsageError("--device is the numeric id of the device");
  return o;
}

/** readFirstLine reads a stream up to the first newline (without it, without a trailing CR) or its end. */
export async function readFirstLine(stream) {
  let text = "";
  for await (const chunk of stream) {
    text += chunk.toString("utf8");
    const nl = text.indexOf("\n");
    if (nl >= 0) {
      text = text.slice(0, nl);
      break;
    }
  }
  return text.replace(/\r$/, "");
}

/** resolvePassword takes TASMOFLEET_SMOKE_PASSWORD, else the first line of stdin (readLine); empty is a UsageError. */
export async function resolvePassword(env, readLine) {
  const password = env.TASMOFLEET_SMOKE_PASSWORD || (await readLine());
  if (!password) throw new UsageError(`no password: ${PASSWORD_HELP}`);
  return password;
}

/** realpathLoose resolves the symlinks of the part of p that exists and appends the rest unchanged. */
function realpathLoose(p) {
  const rest = [];
  let cur = resolve(p);
  while (!existsSync(cur)) {
    rest.unshift(cur.slice(dirname(cur).length + 1));
    cur = dirname(cur);
  }
  return join(realpathSync(cur), ...rest);
}

/** checkOutDir refuses an output directory inside the repository (screenshots are never committed; ui-review). */
export function checkOutDir(out, repoRoot) {
  const repo = realpathSync(repoRoot);
  const dir = realpathLoose(out);
  if (dir === repo || dir.startsWith(repo + sep)) throw new UsageError("the output directory is inside the repository; screenshots are never committed, pick a path outside it");
}

/** createOutDir makes the directory; an existing one must be empty, so a run never overwrites an earlier one. */
export function createOutDir(dir) {
  mkdirSync(dirname(dir), { recursive: true });
  try {
    mkdirSync(dir);
  } catch (e) {
    if (e.code !== "EEXIST") throw e;
    if (readdirSync(dir).length > 0) throw new UsageError("the output directory already holds files; a run never overwrites an earlier one");
  }
}

/** defaultOutDir is <cache>/tasmofleet-smoke/<UTC time>, outside every repository. */
export function defaultOutDir(env, now = new Date(), home = homedir()) {
  const h = env.HOME || home;
  const cache = env.XDG_CACHE_HOME && isAbsolute(env.XDG_CACHE_HOME) ? env.XDG_CACHE_HOME : join(h, ".cache");
  const stamp = now.toISOString().replace(/\.\d+Z$/, "Z").replace(/[-:]/g, "");
  return join(cache, "tasmofleet-smoke", stamp);
}

/** pagePath is the path of a page with the device id filled in. */
export function pagePath(page, deviceId) {
  return page.path.replace("<id>", deviceId);
}

/** shotName is the file name of the i-th page's screenshot: 01-devices.png, 02-device-42.png. */
export function shotName(i, page, deviceId) {
  const name = page.path.includes("<id>") ? `${page.name}-${deviceId}` : page.name;
  return `${String(i + 1).padStart(2, "0")}-${name}.png`;
}
