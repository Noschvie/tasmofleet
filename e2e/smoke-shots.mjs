// Takes the screenshots of the release smoke check (docs/dev/releasing.md, #822) from a RUNNING instance:
//   TASMOFLEET_SMOKE_PASSWORD=... node smoke-shots.mjs --url https://host/base --user name [--device id] [--out dir]
// or `make smoke-shots SMOKE_URL=... SMOKE_USER=...` from the repository root. The password is the environment
// variable, else the first line of stdin, never an argument. Signs in once through the local sign-in form, opens
// the five pages at 1280 px in a headless browser (GET only), writes one PNG each into a directory outside the
// repository and signs out again. It takes the screenshots, it does not judge them: the releaser looks at each.
// Exit 0: five files written. Exit 1: a step failed (named on stderr). Exit 2: wrong usage.
import { chromium } from "@playwright/test";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  PAGES,
  UsageError,
  VIEWPORT,
  checkOutDir,
  createOutDir,
  defaultOutDir,
  pagePath,
  parseArgs,
  readFirstLine,
  resolvePassword,
  shotName,
} from "./smoke-shots-lib.mjs";

const USAGE = "usage: node smoke-shots.mjs --url <https://host/base> --user <name> [--device <id>] [--out <dir>]\n  password: TASMOFLEET_SMOKE_PASSWORD, else the first line of stdin";
const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

class StepError extends Error {}

// The password, so that no error text (a Playwright call log can quote what was typed) ever carries it.
let secret = "";
const scrub = (text) => (secret ? text.split(secret).join("***") : text);

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  if (opts.help) {
    console.log(USAGE);
    return;
  }
  const out = resolve(opts.out || defaultOutDir(process.env));
  checkOutDir(out, repoRoot);
  const password = await resolvePassword(process.env, () => {
    if (process.stdin.isTTY) throw new UsageError("no password: set TASMOFLEET_SMOKE_PASSWORD or pipe it on stdin (a terminal would echo it)");
    return readFirstLine(process.stdin);
  });

  secret = password;
  const base = new URL(opts.url);
  const basePath = base.pathname.replace(/\/+$/, "");
  const browser = await chromium.launch();
  let page;
  let signedIn = false;
  try {
    const context = await browser.newContext({ viewport: VIEWPORT, colorScheme: "light" });
    page = await context.newPage();
    page.setDefaultTimeout(30_000);

    // Step "sign-in": one attempt, no retry (the sign-in limit counts failures per account).
    await page.goto(`${opts.url}/login`);
    const form = page.locator('form[action$="/login/local"]');
    if ((await form.count()) === 0)
      throw new StepError(`sign-in: ${opts.url}/login has no local sign-in form (an instance with single sign-on or a forward-auth proxy only, or local sign-in switched off); reach the instance directly or enable the local sign-in`);
    await form.locator('input[name="username"]').fill(opts.user);
    await form.locator('input[name="password"]').fill(password);
    const [answer] = await Promise.all([page.waitForNavigation(), form.getByRole("button", { name: "Sign in" }).click()]);
    if (new URL(page.url()).pathname.startsWith(`${basePath}/login`)) {
      const text = (await page.getByRole("alert").first().innerText({ timeout: 2_000 }).catch(() => "")).replace(/\s+/g, " ").trim();
      throw new StepError(`sign-in: refused (HTTP ${answer?.status() ?? "?"}${text ? `: ${text}` : ""}); not retried`);
    }
    signedIn = true;

    // Step "device": the first device the list's own online filter shows (no health logic copied here).
    let deviceId = opts.device;
    if (!deviceId) {
      const listed = `${opts.url}/devices?health=online`;
      await open(page, listed, `${basePath}/devices`);
      const hrefs = await page.locator("a[href]").evaluateAll((as) => as.map((a) => a.getAttribute("href")));
      const re = new RegExp(`^${basePath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/devices/(\\d+)$`);
      deviceId = hrefs.map((h) => re.exec(h ?? "")?.[1]).find(Boolean);
      if (!deviceId) throw new StepError(`device: ${listed} lists no online device; bring one online or name one with --device`);
    }

    // The directory exists only once the sign-in has worked: a refused run leaves no files.
    createOutDir(out);
    const themes = new Set();
    for (const [i, p] of PAGES.entries()) {
      const path = pagePath(p, deviceId);
      const theme = await open(page, `${opts.url}${path}`, `${basePath}${path}`, p.name);
      themes.add(theme);
      const file = join(out, shotName(i, p, deviceId));
      await shoot(page, file);
      console.log(`${file}  ${path}  theme=${theme}`);
    }
    if (themes.has("dark"))
      console.error("warning: the account's stored theme is dark, the screenshots are not light; set the theme to Auto in the user menu and run again");
  } finally {
    if (signedIn) await signOut(page, opts.url);
    await browser.close();
  }
}

/**
 * shoot makes the window as tall as the page, then takes the whole page. A full-page shot of a shorter window
 * would paint the pinned status bar into the middle of the page (it sits at the window's bottom edge); in a
 * window as tall as the page it sits at the bottom, as it does for a person who scrolls to the end.
 */
async function shoot(page, file) {
  await page.setViewportSize(VIEWPORT);
  const height = await page.evaluate(() => document.documentElement.scrollHeight);
  await page.setViewportSize({ width: VIEWPORT.width, height: Math.min(Math.max(VIEWPORT.height, height), 16_000) });
  await page.screenshot({ path: file, fullPage: true });
  await page.setViewportSize(VIEWPORT);
}

/** open loads a page and fails loud on a status other than 200 or a bounce away from the address; returns the page's theme. */
async function open(page, url, wantPath, name = "page") {
  const res = await page.goto(url);
  const got = new URL(page.url()).pathname;
  if (got !== wantPath) throw new StepError(`${name}: ${url} led to ${got} (signed out, or must change the password first?)`);
  if (res?.status() !== 200) throw new StepError(`${name}: ${url} answered HTTP ${res?.status()}`);
  await page.evaluate(() => document.fonts.ready);
  return (await page.locator("html").getAttribute("data-theme")) ?? "auto";
}

/** signOut ends the server-side session (POST /logout with the page's CSRF token); a failure is reported, not fatal. */
async function signOut(page, url) {
  try {
    await page.goto(`${url}/devices`);
    const csrf = page.locator('form[action$="/logout"] input[name="csrf"]').first();
    if ((await csrf.count()) === 0) return; // no session left (it ended or the page bounced): nothing to end
    const res = await page.request.post(`${url}/logout`, { form: { csrf: await csrf.getAttribute("value") } });
    if (!res.ok()) console.error(`warning: sign-out answered HTTP ${res.status()}; the session ends at its normal expiry`);
  } catch (e) {
    console.error(`warning: sign-out failed (${scrub(e.message.split("\n")[0])}); the session ends at its normal expiry`);
  }
}

main().catch((e) => {
  if (e instanceof UsageError) {
    console.error(`smoke-shots: ${e.message}\n${USAGE}`);
    process.exit(2);
  }
  if (e instanceof StepError) {
    console.error(`smoke-shots: ${scrub(e.message)}`);
    process.exit(1);
  }
  console.error(`smoke-shots: unexpected error: ${scrub(String(e.message ?? e).split("\n")[0])}`);
  process.exit(1);
});
