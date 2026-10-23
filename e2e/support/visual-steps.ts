import { readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Cookie, Page } from "@playwright/test";
import type { Step } from "./visual-entries";
import { localPassword } from "./visual-stack";

// Steps reach a state the way the prototype's renderer did (ref/HOWTO.md, tools/render.mjs):
// text and aria-label lookups, a pause after each action. A step the app cannot do yet (a
// banner, a dialog that does not exist before its package lands) fails with the owner's name
// instead of silently capturing the wrong state.

/** NotYet marks a custom step whose page or feature arrives with a later work package. */
export class NotYet extends Error {
  constructor(step: string, owner: string) {
    super(`step "${step}" is not implemented yet (lands with ${owner})`);
  }
}

const customOwners: Record<string, string> = {
  "live-check-failed": "a fault hook in the visual stack (the policy report fails only when the device list cannot be read; the failed status line and Retry now are covered by TestPolicyLiveLineShowsAFailure)",
  "form-errors": "the owning page's package",
  "restore-failed": "a fault hook in the visual stack (a restore of a fixture device cannot be made to fail on demand; the failed state is covered by handler tests)",
};

/** StepVars are the stack's values a custom step may need (the devenv control URL as "control"). */
export type StepVars = Record<string, string>;

export async function runSteps(page: Page, steps: Step[], owner: string, vars: StepVars = {}): Promise<void> {
  for (const step of steps) {
    // Only what is on screen counts, as for the prototype's renderer: the device list renders its
    // table and its phone cards, CSS shows one of them.
    if ("click" in step) {
      const target = page.getByText(step.click, { exact: step.exact ?? false }).filter({ visible: true });
      if (step.optional && (await target.count()) === 0) continue; // the control exists only at some widths
      await target.nth(step.nth ?? 0).click({ timeout: 3_000 });
    } else if ("aria" in step) await page.getByLabel(step.aria, { exact: true }).filter({ visible: true }).nth(step.nth ?? 0).click({ timeout: 3_000 });
    else if ("clickSel" in step) await page.locator(step.clickSel).first().click({ timeout: 3_000 });
    else if ("wait" in step) await page.waitForTimeout(step.wait);
    else if ("preselect" in step) await preselect(page, step.preselect);
    else if (customSteps[step.custom]) await customSteps[step.custom](page, vars);
    else throw new NotYet(step.custom, customOwners[step.custom] ?? owner);
    await page.waitForTimeout(250);
  }
}

/** preselect ticks the first n online devices of the list, rows on a desktop and cards on a phone. */
async function preselect(page: Page, n: number): Promise<void> {
  const boxes = page
    .locator("tr, li, article")
    .filter({ hasText: "Online" })
    .getByRole("checkbox", { name: /^Select / });
  for (let i = 0; i < n; i++) await boxes.nth(i).check({ timeout: 3_000 });
}

/**
 * sseErrors makes live.js see n failed reconnects of the page's event stream, through the same
 * htmx event the SSE extension fires (the stream stays open; nothing else reacts to the event).
 * The prototype's states: attempt 3 is "Reconnecting", the fifth failure in a row is "Offline".
 */
async function sseErrors(page: Page, n: number): Promise<void> {
  await page.evaluate((count) => {
    for (let i = 0; i < count; i++)
      document.body.dispatchEvent(
        new CustomEvent("htmx:sseError", {
          bubbles: true,
          detail: { source: { readyState: 0 } },
        }),
      );
  }, n);
}

/** feed switches the devenv's fake GitHub feed: down, dl-down, slow or up (cmd/tasmofleet-devenv). */
async function feed(page: Page, vars: StepVars, state: "down" | "dl-down" | "slow" | "up"): Promise<void> {
  if (!vars.control) throw new Error("the stack has no devenv control URL");
  const r = await page.request.post(`${vars.control}/fixture/feed/${state}`);
  if (r.status() !== 204) throw new Error(`feed ${state}: HTTP ${r.status()}`);
}

/**
 * freshIndex makes the app check its feed once more without a toast: a step before this one on
 * the same stack may have left the last check failed (github-down).
 */
async function freshIndex(page: Page, vars: StepVars): Promise<void> {
  await feed(page, vars, "up"); // a switch an aborted step left on
  if ((await page.getByRole("alert").filter({ hasText: "not reachable" }).count()) === 0) return;
  await page.evaluate(async () => {
    const f = document.querySelector<HTMLFormElement>("form[data-fw-check]");
    if (f) await fetch(f.action, { method: "POST", body: new URLSearchParams(new FormData(f) as unknown as Record<string, string>) });
  });
  await page.reload();
}

/**
 * c3Uncached brings back the prototype's state of the ESP32-C3 file of the newest release (not
 * cached, Prefetch offered) after an earlier step on the same stack fetched it: it waits for a
 * running prefetch to end and deletes the file.
 */
async function c3Uncached(page: Page): Promise<void> {
  const url = page.url();
  for (let i = 0; i < 20 && (await page.getByText("Fetching…").count()) > 0; i++) {
    await page.waitForTimeout(2_000);
    await page.goto(url);
  }
  const row = page.locator(".fw-crow").filter({ hasText: "tasmota32c3.bin" }).first();
  if ((await row.count()) > 0) {
    await row.locator(".fw-del").click();
    await page.locator("dialog[open]").getByRole("button", { name: "Delete file" }).click();
    await page.waitForLoadState();
    await page.goto(url);
  }
}

/**
 * The prototype's bulk screens show its default selection (DEF in design/v2/template.html) in that
 * order: six online devices, Hallway Dimmer offline and Kitchen Ceiling Light unknown. bulkScope
 * posts these devices from the device list's form (its CSRF token, the ids of the rows' checkboxes)
 * to the action the bar's button would post to, in the prototype's order, which a click on the
 * bar cannot reproduce (it posts in list order).
 */
const bulkRefDevices = [
  "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll",
  "Living Room Ceiling Light",
  "Hallway Dimmer",
  "Laundry Ceiling Light",
  "Kitchen Ceiling Light",
  "Office Heater Plug",
  "Workshop Fan",
  "Workshop Switch",
];

async function bulkScope(page: Page, path: string, fields: Record<string, string> = {}): Promise<void> {
  await Promise.all([
    page.waitForEvent("load"),
    page.evaluate(
      ({ path, names, fields }) => {
        const picker = document.getElementById("picker") as HTMLFormElement;
        const base = (picker.getAttribute("action") ?? "").replace(/\/devices\/bulk\/power$/, "");
        const f = document.createElement("form");
        f.method = "post";
        f.action = base + path;
        const add = (name: string, value: string) => {
          const i = document.createElement("input");
          i.type = "hidden";
          i.name = name;
          i.value = value;
          f.appendChild(i);
        };
        add("csrf", (picker.querySelector('input[name="csrf"]') as HTMLInputElement).value);
        for (const n of names) {
          const box = document.querySelector(`input[name="id"][aria-label="Select ${n}"]`) as HTMLInputElement | null;
          if (!box) throw new Error(`no device "${n}" in the list`);
          add("id", box.value);
        }
        for (const [k, v] of Object.entries(fields)) add(k, v);
        document.body.appendChild(f);
        f.submit();
      },
      { path, names: bulkRefDevices, fields },
    ),
  ]);
}

/** bulkResult runs the scope page's action (through the command preview) and waits for its end. */
async function bulkResult(page: Page): Promise<void> {
  await Promise.all([page.waitForEvent("load"), page.locator(".bk-aside button[type=submit]").click()]);
  const confirm = page.locator(".confirm-card button[type=submit]");
  if (await confirm.count()) await Promise.all([page.waitForEvent("load"), confirm.click()]);
  await page.locator("#bk-result h1").filter({ hasText: /finished/ }).waitFor({ timeout: 90_000 });
}

/**
 * stepup reaches "Confirm it's you" the way a person does: a local admin (the "local" stack; header
 * principals can never confirm) changes a sign-in setting and saves, the server refuses and sends
 * the page back with the dialog open. Nothing is stored. Elsewhere (the users pages) the page's own
 * dialog is opened (stepUp).
 */
async function stepup(page: Page): Promise<void> {
  // the settings route redirects to the sign-in page first: sign in as maren (signIn keeps the
  // session per stack, the sign-in rate limit allows fewer sign-ins than a run has entries)
  if (await page.locator('form[action$="/login/local"]').count()) await signIn(page, "maren", localPassword);
  if (!(await page.locator("form#sysform").count())) return stepUp(page); // the users pages (P9)
  await page.evaluate(() => {
    const f = document.querySelector<HTMLInputElement>('input[name="v.OIDC_GROUPS_CLAIM"]');
    if (f) f.value = "visual-stepup";
  });
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.locator("dialog#stepup[open]").waitFor({ timeout: 3_000 });
}

/**
 * signIn signs in on the sign-in page the route was redirected to (the "local" stack); the form's
 * next field brings the browser back to the entry's route. With reuse, the session cookie of an
 * earlier sign-in of this worker is put into the new context instead: the app allows 20 sign-ins
 * per 15 minutes from one network, fewer than the entries of a run need.
 */
// Kept in a file per app instance: Playwright restarts a worker after every failed comparison,
// which would empty an in-memory cache.
function sessionFile(page: Page): string {
  return join(tmpdir(), `tasmofleet-visual-sessions-${new URL(page.url()).port}.json`);
}
function savedSessions(page: Page): Record<string, Cookie[]> {
  try {
    return JSON.parse(readFileSync(sessionFile(page), "utf8"));
  } catch {
    return {};
  }
}
async function signIn(page: Page, user: string, password: string, reuse = true, key = user): Promise<void> {
  const saved = reuse ? savedSessions(page)[key] : undefined;
  if (saved) {
    await page.context().addCookies(saved);
    await page.reload();
    if (!page.url().includes("/login")) return;
  }
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  if (!password.startsWith("hunter"))
    await page.waitForURL((u) => !/\/login(\/local)?$/.test(u.pathname), { timeout: 10_000 }).catch(async () => {
      throw new Error(`sign-in as ${user} stayed on ${page.url()}: ${(await page.locator("main").innerText()).replace(/\s+/g, " ").slice(0, 200)}`);
    });
  else await page.getByRole("alert").first().waitFor({ timeout: 10_000 });
  if (reuse) writeFileSync(sessionFile(page), JSON.stringify({ ...savedSessions(page), [key]: await page.context().cookies() }));
}

let wrong = 0;

/** setupPassword is the fake password the setup-v-basics entry gives the first admin. */
const setupPassword = "design-fixture-setup-password-1";

/**
 * steppedUp signs maren in with a session of its own whose step-up is done (the actions of the
 * users pages then open their own dialogs): the other entries keep showing the locked page.
 */
async function steppedUp(page: Page): Promise<void> {
  await signIn(page, "maren", localPassword, true, "maren-stepped-up");
  if (await page.locator("dialog.stepup").count()) {
    const back = page.url();
    await stepUp(page);
    await page.locator("dialog.stepup").getByLabel("Password").fill(localPassword);
    await page.locator("dialog.stepup").getByRole("button", { name: "Confirm" }).click();
    await page.waitForURL(/\/users$/, { timeout: 10_000 });
    await page.goto(back);
  }
}

/** stepUp opens the users pages' "Confirm it's you" dialog, as an action of a locked page does. */
async function stepUp(page: Page): Promise<void> {
  const opened = await page.evaluate(() => {
    const d = document.querySelector<HTMLDialogElement>("dialog.stepup");
    d?.showModal();
    return !!d;
  });
  if (!opened) throw new Error("no step-up dialog: the signed-in user cannot confirm a change here (view only behind the header proxy)");
}

/** tickBackups ticks the first n backup rows of the Backups tab (failed rows have no box). */
async function tickBackups(page: Page, n: number): Promise<void> {
  const boxes = page.locator(".bk-row").getByRole("checkbox");
  for (let i = 0; i < n; i++) await boxes.nth(i).check({ timeout: 3_000 });
}

/**
 * startRestore ticks the acknowledgement and starts the restore; the job page follows. The
 * restore screens share one mutating stack and the light shot follows the dark one at once, so the
 * restore the previous shot started may still run: the start is then refused ("Restore not
 * started", 409) and is retried from the confirmation once that restore is over.
 */
async function startRestore(page: Page): Promise<void> {
  const confirm = page.url();
  for (let tries = 0; ; tries++) {
    await page.locator(".rs-ack input").check({ timeout: 3_000 });
    const answer = page.waitForResponse(
      (r) => r.request().method() === "POST" && r.url().endsWith("/restore"),
      { timeout: 10_000 },
    );
    await page
      .getByRole("button", { name: "Restore and restart" })
      .click({ timeout: 3_000 });
    if ((await answer).status() !== 409) {
      await page.waitForURL(/\/restores\/\d+$/, { timeout: 10_000 });
      return;
    }
    if (tries >= 60)
      throw new Error(
        "restore: the start stayed refused for 2 minutes (an earlier restore still running?)",
      );
    await page.waitForTimeout(2_000);
    await page.goto(confirm);
  }
}

/** firmwareOf is the firmware page of the app a release page (N6) belongs to. */
function firmwareOf(releaseURL: string): string {
  return releaseURL.replace(/\/firmware\/releases\/[^?#]*.*$/, "/firmware");
}

/** customSteps are the named steps that exist; the others throw NotYet with their owner. */
const customSteps: Record<string, (page: Page, vars: StepVars) => Promise<void>> = {
  "backups-select-two": (page) => tickBackups(page, 2),
  "prune-dialog": async (page) => {
    await tickBackups(page, 2);
    await page
      .getByRole("link", { name: "Prune now…" })
      .click({ timeout: 3_000 });
    await page.locator("dialog[open]").waitFor({ timeout: 3_000 });
  },
  "replace-preview": (page) =>
    page
      .getByRole("button", { name: "Preview what’s copied" })
      .click({ timeout: 3_000 }),
  "restore-progress": startRestore,
  "restore-done": async (page) => {
    await startRestore(page);
    await page.getByText("Restore finished.").waitFor({ timeout: 120_000 });
  },
  "conn-reconnecting": (page) => sseErrors(page, 3),
  "conn-offline": (page) => sseErrors(page, 5),
  // P9: the local stack's admin maren, a wrong password, an expired session, ali's forced change
  "signin-maren": (page) => signIn(page, "maren", localPassword),
  // a different unknown name per attempt: five failures of one name would delay the sixth
  "signin-error": (page) => signIn(page, `maren${++wrong}`, "hunter1", false),
  "signin-expired": async (page) => {
    await page.goto(page.url() + (page.url().includes("?") ? "&" : "?") + "expired=1");
  },
  "must-change-password": async (page) => {
    await signIn(page, "ali", localPassword);
    await page.getByLabel("One-time password").fill(localPassword);
    await page.getByLabel("New password", { exact: true }).fill("fleet2026");
    await page.getByLabel("Repeat new password").fill("fleet2025");
    await page.getByRole("button", { name: "Save and continue" }).click();
    await page.locator(".ufield-err").first().waitFor({ timeout: 10_000 });
  },
  // the add-user form refused: a taken username, a short password, a mismatched repeat
  "useradd-errors": async (page) => {
    await steppedUp(page);
    await page.goto(page.url().replace(/\/users(\/new)?$/, "/users/new"));
    await page.getByLabel("Username").fill("ali");
    await page.getByText("Set an initial password").click();
    await page.locator('input[name="password"]').fill("short");
    await page.locator('input[name="confirm"]').fill("shorter");
    await page.getByRole("button", { name: "Add user" }).click();
    await page.getByRole("alert").first().waitFor({ timeout: 10_000 });
  },
  "signin-stepped-up": steppedUp,
  // N4: Reset password on the user's page (the step-up is done): the one-time password page
  "user-otp": async (page) => {
    await page.locator(".ud-acts").getByRole("link", { name: "Reset password" }).click();
    await page.locator("dialog[open]").getByRole("button", { name: "Reset password" }).click();
    await page.getByRole("heading", { name: /^Password reset for / }).waitFor({ timeout: 10_000 });
  },
  // N12: the first admin's password through the setup link, then the wizard's last step. A later
  // shot finds the link spent (the page sends it to the sign-in) and signs in with that password.
  "setup-basics": async (page) => {
    if (await page.locator('input[name="token"]').count()) {
      await page.locator('input[name="password"]').fill(setupPassword);
      await page.locator('input[name="confirm"]').fill(setupPassword);
      await page.getByRole("button", { name: "Create account and continue" }).click();
      await page.waitForURL(/\/setup$/, { timeout: 10_000 });
    } else {
      await signIn(page, "admin", setupPassword);
      await page.goto(new URL("/setup", page.url()).href);
    }
    await page.getByRole("heading", { name: "Basic settings" }).waitFor({ timeout: 10_000 });
  },
  // the prototype's filtered variant: Chip ESP32 and Tag lights (each select applies at once)
  "filter-chips": async (page) => {
    await selectAndWait(page, "Chip", "esp32");
    await selectAndWait(page, "Tag", "lights");
  },
  "form-errors": formErrors,
  // step 4 of the new update: the preselected devices (every outdated one) through "Review update"
  "update-preview": async (page) => {
    await page.getByRole("button", { name: "Review update" }).click();
    await page.getByRole("heading", { name: /^4 · Preview/ }).waitFor({ timeout: 90_000 });
  },
  // the page in the moment after "Check for new releases now" (firmware.js): the form submission is
  // cancelled after the page has reacted to it, so the state stays (a held request would block the
  // capture, which waits for the navigation)
  "firmware-checking": async (page) => {
    await page.evaluate(() => {
      window.addEventListener("submit", (e) => e.preventDefault());
      document.querySelector<HTMLButtonElement>("form[data-fw-check] button[type=submit]")?.click();
    });
    await page.getByText("Checking…", { exact: true }).waitFor({ timeout: 5_000 });
  },
  // "Check for new releases now" on a feed that answers (an earlier github-down on this stack may
  // have left the last check failed)
  "check-now": async (page, vars) => {
    await freshIndex(page, vars);
    await page.getByRole("button", { name: "Check for new releases now" }).click();
  },
  // the feed fails one check: the page shows the banner, the old list and Try again
  "github-down": async (page, vars) => {
    await feed(page, vars, "down");
    try {
      // "Check for new releases now", or "Try again" after an earlier mode left the check failed
      await page.locator("form.fw-check-form button[type=submit]").click({ timeout: 10_000 });
      await page.getByRole("alert").filter({ hasText: "not reachable" }).waitFor({ timeout: 30_000 });
    } finally {
      await feed(page, vars, "up");
    }
  },
  // downloads are held back; the prefetch of the fleet runs and the page shows the file that is fetching
  "prefetch-progress": async (page, vars) => {
    await freshIndex(page, vars);
    if ((await page.getByText("Fetching…").count()) > 0) return; // the run of an earlier mode is still on
    await c3Uncached(page);
    await feed(page, vars, "slow");
    const url = page.url();
    await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
    await page.waitForURL(/\/firmware\/prefetch\//);
    await page.goto(url);
    await page.getByText("Fetching…").first().waitFor({ timeout: 10_000 });
  },
  // the download of the ESP32-C3 file fails: its row says so and offers Retry
  "prefetch-failed": async (page, vars) => {
    await freshIndex(page, vars);
    await c3Uncached(page);
    await feed(page, vars, "dl-down");
    try {
      await page.getByRole("button", { name: "Prefetch tasmota32c3.bin", exact: true }).click();
      await page.getByText("Download failed").waitFor({ timeout: 30_000 });
    } finally {
      await feed(page, vars, "up");
    }
  },
  // the release page (N6) has no cached-files list: the C3 file is made uncached on the firmware
  // page, then the release page's own Prefetch for my fleet / Prefetch is used
  "release-prefetch-progress": async (page, vars) => {
    const url = page.url();
    await page.goto(firmwareOf(url));
    await freshIndex(page, vars);
    await c3Uncached(page);
    await page.goto(url);
    if ((await page.getByText("Fetching…").count()) > 0) return; // the run of an earlier mode is still on
    await feed(page, vars, "slow");
    await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
    await page.waitForURL(/\/firmware\/prefetch\//);
    await page.goto(url);
    await page.getByText("Fetching…").first().waitFor({ timeout: 10_000 });
  },
  "release-prefetch-failed": async (page, vars) => {
    const url = page.url();
    await page.goto(firmwareOf(url));
    await freshIndex(page, vars);
    await c3Uncached(page);
    await page.goto(url);
    await feed(page, vars, "dl-down");
    try {
      await page.getByRole("button", { name: "Prefetch tasmota32c3.bin", exact: true }).click();
      await page.getByText("Download failed").waitFor({ timeout: 30_000 });
    } finally {
      await feed(page, vars, "up");
    }
  },
  // the Delete of the newest release's tasmota.bin.gz opens its confirmation dialog; clicked in the
  // page, so the list behind the dialog is not scrolled to the row (the prototype's capture is not)
  "delete-cached-file": async (page) => {
    await page.evaluate(() => {
      const row = [...document.querySelectorAll(".fw-crow")].find((r) => r.querySelector(".fw-c-f")?.textContent?.trim() === "tasmota.bin.gz");
      row?.querySelector<HTMLElement>(".fw-del")?.click();
    });
    await page.locator("dialog[open]").waitFor();
  },
  "manual-address-error": manualAddressError,
  "discovery-off": discoveryOff,
  "scan-finished": (page) => scan(page, "127.0.0.0/24", true),
  "scan-running": (page) => scan(page, "127.0.0.0/22", false),
  "import-plan": importPlan,
  "import-result": async (page) => {
    await importPlan(page);
    await page.getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByText("Import finished.").waitFor({ timeout: 60_000 });
  },
  "bulk-scope-cmd": async (page) => {
    await bulkScope(page, "/devices/bulk/command");
    await page.locator('textarea[name="command"]').fill("Backlog SetOption19 0; TelePeriod 300");
  },
  "bulk-scope-backup": async (page) => {
    await bulkScope(page, "/devices/bulk/backup");
    await page.locator('input[name="label"]').fill("before winter");
  },
  "bulk-scope-restart": (page) => bulkScope(page, "/devices/bulk/restart"),
  "bulk-scope-on": (page) => bulkScope(page, "/devices/bulk/power", { action: "ON" }),
  "bulk-scope-off": (page) => bulkScope(page, "/devices/bulk/power", { action: "OFF" }),
  "bulk-result": bulkResult,
  stepup,
  // N3 / N21: the policy page's live-check states and the import preview
  "live-check-paused": (page) => liveCheck(page, true),
  "live-check-running": (page) => liveCheck(page, false),
  "policy-import": policyImport,
};

/** selectAndWait picks a value in a labelled select that submits its form, and waits for the new page. */
async function selectAndWait(page: Page, label: string, value: string): Promise<void> {
  await Promise.all([page.waitForURL((u) => u.searchParams.has(label === "Chip" ? "family" : "tag")), page.getByRole("combobox", { name: label, exact: true }).selectOption(value)]);
  await page.waitForLoadState("load");
}

/**
 * formErrors submits the page's form with values the server refuses, per page. The edit form
 * (P3): an address with a path, which the inventory refuses without touching the device (the
 * prototype's "192.168.20.300"); the name stays, the browser would block an empty required one.
 */
async function formErrors(page: Page): Promise<void> {
  if (new URL(page.url()).pathname.endsWith("/setup/account")) {
    // setup step 1 (N12): the prototype's name, a password that is too short, a repeat that differs;
    // the server answers the first refused field (the repeat). The link stays usable.
    await page.getByLabel("Display name").fill("Maren Koch");
    await page.locator('input[name="password"]').fill("fleet2026");
    await page.locator('input[name="confirm"]').fill("fleet2025");
    await Promise.all([page.waitForNavigation(), page.getByRole("button", { name: "Create account and continue" }).click()]);
    return;
  }
  if (/\/devices\/\d+\/edit$/.test(new URL(page.url()).pathname)) {
    await page.locator('input[name="address"]').fill("192.168.20.300/x");
    await Promise.all([page.waitForNavigation(), page.getByRole("button", { name: "Save changes" }).click()]);
    return;
  }
  throw new NotYet("form-errors", customOwners["form-errors"]);
}

/** manualAddressError submits the prototype's mistyped address ("attic heater"): the field error state. */
async function manualAddressError(page: Page): Promise<void> {
  await page.getByLabel("IP address or hostname").fill("attic heater");
  await page.getByRole("button", { name: "Add device" }).click();
  await page.getByText("Enter an IP address or hostname without spaces").waitFor();
}

/** csrf reads the page's CSRF token from the htmx default headers. */
async function csrf(page: Page): Promise<string> {
  return page.evaluate(() => JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"] ?? "");
}

/** discoveryOff switches MQTT discovery off in the settings (the entry runs on its own stack) and reloads. */
async function discoveryOff(page: Page): Promise<void> {
  const res = await page.request.post(new URL("/settings/system", page.url()).toString(), {
    form: { csrf: await csrf(page), "v.DISCOVERY_MQTT": "false" },
    maxRedirects: 0,
  });
  if (res.status() !== 303) throw new Error(`switching discovery off: HTTP ${res.status()}`);
  await page.reload();
}

/**
 * scan runs a network scan of a loopback range. The stack's simulated devices listen on other
 * ports than 80, so a scan finds no Tasmota device, and it ends within a second: the running state
 * is captured only if the page is quicker (plan, P6 exceedances).
 */
async function scan(page: Page, range: string, waitDone: boolean): Promise<void> {
  await page.getByLabel("Subnet").fill(range);
  await page.getByRole("button", { name: "Scan now" }).click();
  await page.waitForURL(/\/discovery\/scan\/[^/]+$/);
  if (waitDone) await page.getByText("Scan finished.").waitFor({ timeout: 60_000 });
}

/** deviceAddress is a device's host:port as the device list's table has it (also when a phone shows cards). */
async function deviceAddress(page: Page, name: string): Promise<string> {
  const cell = page.locator(`tr:has(a:text-is("${name}"))`).first().locator('td[data-label="Address"]');
  return ((await cell.getAttribute("title")) ?? "").trim(); // host:port (the cell text drops the port)
}

/**
 * importPlan uploads a devices.csv like the prototype's plan: four new devices, two that match
 * devices of the fleet by address (merge), one outside the device networks and one duplicate
 * (reject). The new ones get loopback addresses inside the stack's device networks.
 */
async function importPlan(page: Page): Promise<void> {
  const back = page.url();
  await page.goto(new URL("/devices", back).toString());
  const merge = [await deviceAddress(page, "Kitchen Plug"), await deviceAddress(page, "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll")];
  await page.goto(back);
  const line = (i: number, name: string, addr: string, topic: string) => {
    const [host, port] = addr.split(":");
    return `${i},"${name}",${host},,,bulb_1,1,1,0,0,1,${port ?? 80},"${name}",0,${topic && topic + "_" + net.replaceAll(".", "_")},0`;
  };
  // A fresh range per call: the six captures of an entry share one stack, and an address
  // imported by the first capture would merge (not create) in the next one.
  const net = `127.${1 + Math.floor(Math.random() * 250)}.${1 + Math.floor(Math.random() * 250)}`;
  const csv = [
    line(1, "Shed Light", `${net}.140`, "shed_light"),
    line(2, "Guest Room Plug", `${net}.141`, "guest_plug"),
    line(3, "Garage Door Relay", `${net}.143`, "garage_relay"),
    line(4, "Pool Pump Switch", `${net}.145`, "pool_pump"),
    line(5, "Kitchen Plug", merge[0], ""),
    line(6, "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll", merge[1], ""),
    line(7, "Old Sonoff", "192.168.200.7", "old_sonoff"),
    line(8, "Shed Light (copy)", `${net}.140`, "shed_light_copy"),
  ].join("\n") + "\n";
  await page.locator('input[type="file"][name="files"]').setInputFiles({ name: "devices.csv", mimeType: "text/csv", buffer: Buffer.from(csv) });
  await page.getByText("Import plan").waitFor({ timeout: 30_000 });
}

/**
 * liveCheck puts the policy live check into the wanted state (paused or running) through the
 * page's own Pause / Resume, then reloads the page without the confirmation. The state is
 * global, so the entries that use it run on the mutating stack and each sets what it needs.
 */
async function liveCheck(page: Page, paused: boolean): Promise<void> {
  const button = page.getByRole("button", { name: paused ? "Pause" : "Resume", exact: true });
  if ((await button.count()) > 0) {
    await Promise.all([page.waitForNavigation(), button.first().click({ timeout: 3_000 })]);
  }
  await page.goto(page.url().replace(/\?.*$/, ""));
  await page.getByRole("button", { name: paused ? "Resume" : "Pause", exact: true }).waitFor({ timeout: 5_000 });
}

/**
 * policyImport opens the Import tab and checks a file like the prototype's: the exported rules
 * with one expected value changed, one severity raised and one new rule.
 */
async function policyImport(page: Page): Promise<void> {
  const res = await page.request.get(new URL("/policy/io/export", page.url()).href);
  const rules = (await res.json()) as Array<Record<string, unknown>>;
  if (rules.length > 0) rules[0] = { ...rules[0], want: "600" };
  if (rules.length > 1) rules[1] = { ...rules[1], severity: "warn" };
  rules.push({ name: "Local timezone and NTP", command: "Timezone", want: "99", severity: "info" });
  await page.goto(new URL("/policy/io?tab=import", page.url()).href);
  await page.locator('textarea[name="json"]').fill(JSON.stringify(rules, null, 2));
  await Promise.all([page.waitForNavigation(), page.getByRole("button", { name: "Check import" }).click()]);
}
