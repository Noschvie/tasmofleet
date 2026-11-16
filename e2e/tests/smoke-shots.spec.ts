import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The release smoke check's screenshot script (#822, e2e/smoke-shots.mjs) against a real instance. Why it
// matters: the smoke check exists because pages that answered 200 were broken (0.36.0); a script that takes
// the wrong pages, the wrong device, a pre-sign-in page or leaves a session behind would make that check
// look done while it is not.
const script = resolve(__dirname, "../smoke-shots.mjs");
const password = "e2e-fake-password-1";
let app: App;
let env: Devenv;
let online: string; // the simulated device that answers
let outRoot: string;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  outRoot = mkdtempSync(join(tmpdir(), "smoke-shots-e2e-"));
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32",
      TASMOFLEET_ADMIN_GROUPS: "Tasmofleet Admins",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  online = Object.keys(env.devices).sort()[0];
  // "aaa-offline" sorts before every simulated device and nothing listens at its address: a script that
  // took the first device of the plain list (not the online filter) would shoot this one.
  const fleet = { "aaa-offline": { addr: "127.0.0.1:9", topic: "aaa-offline", profile: "" }, [online]: env.devices[online] };
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(fleet)]);
  for (const user of ["smoke", "smokestdin", "smokewrong"]) app.cli(["user", "passwd", "--create", "--role", "admin", user], password + "\n");
  app.cli(["user", "passwd", "--create", "--role", "admin", "auditor"], password + "\n");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
  rmSync(outRoot, { recursive: true, force: true });
});

/** run starts the script as a child process; the password goes in through the environment or stdin, never an argument. */
function run(args: string[], opts: { env?: string; stdin?: string } = {}) {
  const childEnv = { ...process.env, TASMOFLEET_SMOKE_PASSWORD: opts.env ?? "" };
  if (opts.env === undefined) delete (childEnv as Record<string, string | undefined>).TASMOFLEET_SMOKE_PASSWORD;
  const r = spawnSync("node", [script, "--url", app.url, ...args], { env: childEnv, input: opts.stdin ?? "", encoding: "utf8", timeout: 90_000 });
  return { code: r.status, out: r.stdout, err: r.stderr };
}

/** pngWidth reads the width from the IHDR chunk. */
const pngWidth = (file: string) => readFileSync(file).readUInt32BE(16);

async function lastShotsOf(out: string) {
  return readdirSync(out).sort();
}

test("the five pages are shot at 1280 px, the device is the online one, and the session ends", async ({ page }) => {
  test.setTimeout(150_000);
  // wait until the simulator counts as online, so the script's online filter has something to find
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("auditor");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(async () => {
    await page.goto(app.url + "/devices?health=online");
    await expect(page.getByRole("link", { name: online })).toBeVisible();
  }).toPass({ timeout: 60_000 });

  const out = join(outRoot, "env");
  const r = run(["--user", "smoke", "--out", out], { env: password });
  expect(r.code, r.err).toBe(0);
  const files = await lastShotsOf(out);
  expect(files).toHaveLength(5);
  expect(files.map((f) => f.replace(/-\d+\.png$/, ".png").replace(/^\d+-/, ""))).toEqual(["devices.png", "device.png", "transactions.png", "devices-new.png", "settings-system.png"]);
  for (const f of files) expect(pngWidth(join(out, f)), `${f} is as wide as the viewport (no horizontal overflow)`).toBe(1280);
  expect(r.out + r.err).not.toContain(password);

  // the device page is the online simulator's, not the offline entry that the plain list shows first
  const id = /02-device-(\d+)\.png/.exec(files[1])![1];
  await page.goto(`${app.url}/devices/${id}`);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(online);
  await expect(page.getByRole("heading", { level: 1 })).not.toContainText("aaa-offline");

  // sign-out: the audit log has the session's end (an unfinished run would leave the session until it expires)
  await page.goto(`${app.url}/audit?actor=smoke`);
  await expect(page.getByText("Sign-out").first()).toBeVisible();
});

test("the password can come on stdin, and a named device is shot", async () => {
  const out = join(outRoot, "stdin");
  const r = run(["--user", "smokestdin", "--device", "1", "--out", out], { stdin: password + "\nsecond line\n" });
  expect(r.code, r.err).toBe(0);
  expect((await lastShotsOf(out))[1]).toBe("02-device-1.png");
});

// Why: a typo must cost one attempt of the sign-in limit, not several, and a refused run must leave no files
// that look like a result. The password never reaches the output.
test("a wrong password stops after one attempt with the page's message and writes nothing", async () => {
  const out = join(outRoot, "wrong");
  const r = run(["--user", "smokewrong", "--out", out], { env: "not-the-password-xx" });
  expect(r.code).toBe(1);
  expect(r.err).toContain("sign-in: refused");
  expect(r.err).toContain("Wrong username or password");
  expect(r.err + r.out).not.toContain("not-the-password-xx");
  expect(existsSync(out)).toBe(false);
});

test("a device that does not exist fails loud with its page's status", async () => {
  const r = run(["--user", "smoke", "--device", "99999", "--out", join(outRoot, "missing")], { env: password });
  expect(r.code).toBe(1);
  expect(r.err).toMatch(/device.*HTTP 404/);
});

test("an instance without the local sign-in form is named as such", async () => {
  const sso = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32", TASMOFLEET_ADMIN_GROUPS: "x", TASMOFLEET_LOCAL_LOGIN: "false" } });
  try {
    const r = spawnSync("node", [script, "--url", sso.url, "--user", "smoke", "--out", join(outRoot, "nolocal")], { env: { ...process.env, TASMOFLEET_SMOKE_PASSWORD: password }, encoding: "utf8", timeout: 60_000 });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("no local sign-in form");
  } finally {
    await sso.stop();
  }
});

test("an output directory inside the repository and a password argument are refused before anything starts", async () => {
  const inside = run(["--user", "smoke", "--out", join(__dirname, "shots")], { env: password });
  expect(inside.code).toBe(2);
  expect(inside.err).toContain("inside the repository");
  const arg = run(["--user", "smoke", "--password", "x"], { env: password });
  expect(arg.code).toBe(2);
});
