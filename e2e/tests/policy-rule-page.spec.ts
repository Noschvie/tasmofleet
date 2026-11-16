import { test, expect, type Page } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// The rule page (/policy/rules/{id}) and the New rule page (/policy/rules/new), #197: every route
// to a new rule (template, any command, copy from a device) opens an unsaved draft; Save lands on
// the rule's own address, which keeps naming that rule when the list is reordered.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  expect(
    app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]),
  ).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = {
  "X-authentik-username": "olivia",
  "X-authentik-groups": "ops",
};
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// UI_SHOTS_DIR set: every screenshot at 375 and 1280 px, light and dark.
async function shot(page: Page, name: string) {
  const shots = process.env.UI_SHOTS_DIR;
  if (!shots) return;
  const size = page.viewportSize() ?? { width: 1280, height: 720 };
  for (const [w, h] of [
    [375, 800],
    [1280, 900],
  ]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width: w, height: h });
      await page.emulateMedia({ colorScheme: scheme });
      await page.screenshot({
        path: `${shots}/${name}-${w}-${scheme}.png`,
        fullPage: true,
      });
    }
  }
  await page.setViewportSize(size);
  await page.emulateMedia({ colorScheme: null });
}

const savedRuleID = (url: string) => new URL(url).searchParams.get("rule")!;

test("a new rule from a template is a draft until Save, then lives at its own id that survives a reorder", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new");
  await shot(page, "new-rule");
  await page.getByRole("link", { name: /Local timezone/ }).click();
  await expect(page).toHaveURL(/\/policy\/rules\/new\?template=timezone$/);
  await expect(page.locator("input[name='command']")).toHaveValue("Timezone");
  await expect(page.locator("input[name='want']")).toHaveValue("99");
  await expect(
    page.getByText("Nothing is saved until you press Save rule"),
  ).toBeVisible();
  await shot(page, "draft-from-template");

  await page.getByRole("button", { name: "Save rule" }).click();
  // Save ends the editor on the overview: the flash names the rule and its row is marked
  await expect(page).toHaveURL(/\/policy\?ok=saved&rule=r[a-z0-9]{2,15}$/);
  const first = savedRuleID(page.url());
  await expect(page.getByText("Rule “Local timezone” saved")).toBeVisible();
  await expect(page.locator("tr.is-saved")).toHaveCount(1);
  await expect(page.locator("tr.is-saved")).toContainText("Local timezone");
  await page.goto(app.url + "/policy/rules/" + first);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Local timezone",
  );
  await expect(page.locator(".dev-tile")).toHaveCount(4);
  await shot(page, "rule-page");

  // a second rule through "Any command"
  await page.goto(app.url + "/policy/rules/new");
  await page.locator("#any-command").fill("LedState");
  await page.getByRole("button", { name: "Continue" }).first().click();
  await expect(page).toHaveURL(/\/policy\/rules\/new\?command=LedState$/);
  await page.locator("input[name='want']").fill("3");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved&rule=r[a-z0-9]{2,15}$/);
  const second = savedRuleID(page.url());
  expect(second).not.toBe(first);

  // reorder: the second rule goes first. The first rule's address still names the timezone rule.
  await page.goto(app.url + "/policy/rules/" + second);
  const csrf = await page.locator("input[name='csrf']").first().inputValue();
  const version = await page
    .locator("input[name='version']")
    .first()
    .inputValue();
  const res = await page.request.post(app.url + "/policy/rules/order", {
    headers: admin,
    form: { csrf, version, order: "1,0" },
    maxRedirects: 0,
  });
  expect(res.status()).toBe(303);
  await page.goto(app.url + "/policy/rules/" + first);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Local timezone",
  );
  await page.goto(app.url + "/policy/rules/" + second);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "LedState",
  );
  await page.goto(app.url + "/policy/rules/rzzzzzzzzzz");
  await expect(page.getByText("This page doesn't exist")).toBeVisible();

  // editing the rule keeps its id; Save lands on the overview with that rule marked
  await page.goto(app.url + "/policy/rules/" + first);
  await page.locator("textarea[name='note']").fill("local time everywhere");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(
    new RegExp(`/policy\\?ok=saved&rule=${first}$`),
  );
  await page.goto(app.url + "/policy/rules/" + first);
  await expect(page.locator("textarea[name='note']")).toHaveValue(
    "local time everywhere",
  );
});

test("Any command refuses what must never be sent, with the reason", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new");
  for (const [bad, why] of [
    ["Status0", "Status commands are never sent automatically."],
    ["MqttPassword", "Secret settings cannot be checked."],
  ] as const) {
    await page.locator("#any-command").fill(bad);
    await page.getByRole("button", { name: "Continue" }).first().click();
    await expect(page.locator("#err-any-command")).toHaveText(why);
    await expect(page.locator("#any-command")).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(
      "New rule",
    );
    if (bad === "Status0") await shot(page, "any-command-refused");
  }
});

test("Copy from a device sets the expected value and says where it came from", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new");
  await page.locator("#copy-device").selectOption({ label: "plug-4m-http" });
  await page.locator("#copy-command").fill("LedState");
  await page.getByRole("button", { name: "Continue" }).last().click();
  await expect(
    page.getByText(/Copied from plug-4m-http · read live now/),
  ).toBeVisible({ timeout: 30_000 });
  await expect(page.locator("input[name='want']")).toHaveValue(/^\d+$/);
  await expect(page.locator("input[name='command']")).toHaveValue("LedState");
  await shot(page, "draft-from-copy");
});

test(
  "Copy from a device never sends a refused command",
  { tag: "@core" },
  async ({ page }) => {
    // The simulator's command log is not reachable from the browser, so the page is what is
    // asserted here (a refused command ends on New rule with its reason, nothing opens);
    // internal/web TestRuleNewRefusesCommandsWithTheReasonAndSendsNothing counts the device reads.
    await page.setExtraHTTPHeaders(admin);
    for (const [bad, why] of [
      ["Status0", "Status commands are never sent automatically."],
      ["MqttPassword", "Secret settings cannot be checked."],
    ] as const) {
      await page.goto(app.url + "/policy/rules/new");
      await page
        .locator("#copy-device")
        .selectOption({ label: "plug-4m-http" });
      await page.locator("#copy-command").fill(bad);
      await page.getByRole("button", { name: "Continue" }).last().click();
      await expect(page.locator("#err-copy")).toHaveText(why);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(
        "New rule",
      );
    }
  },
);

test(
  "a person can click Continue while the command suggestion list is open (#867)",
  { tag: "@core" },
  async ({ page }) => {
    // Why: the suggestion list opens a moment after the last keystroke and used to float over the
    // Continue button of the form it belongs to. A click on Continue then landed on the list: it
    // picked a suggestion (replacing the typed command) and the form was not sent. Playwright
    // reported this as ".cmd-sug-f intercepts pointer events" (#860, #867). Here the list is
    // open first, then the pointer goes to where the button is, as a person's does; there is no
    // actionability retry that could hide the overlap.
    await page.setExtraHTTPHeaders(admin);
    for (const [width, height] of [
      [1280, 900],
      [375, 800],
    ]) {
      await page.setViewportSize({ width, height });
      for (const [field, err] of [
        ["any-command", "err-any-command"],
        ["copy-command", "err-copy"],
      ]) {
        await page.goto(app.url + "/policy/rules/new");
        await page
          .locator("#copy-device")
          .selectOption({ label: "plug-4m-http" });
        await page.locator("#" + field).fill("Status0");
        await expect(page.locator(`#${field} + .cmd-sug`)).toBeVisible();
        const button = page
          .locator("form")
          .filter({ has: page.locator("#" + field) })
          .getByRole("button", { name: "Continue" });
        await button.scrollIntoViewIfNeeded();
        const at = (await button.boundingBox())!;
        await page.mouse.click(at.x + at.width / 2, at.y + at.height / 2);
        await expect(page.locator("#" + err)).toHaveText(
          "Status commands are never sent automatically.",
        );
        await expect(page.locator("#" + field)).toHaveValue("Status0");
      }
    }
  },
);

test(
  "the command suggestion list never stays open but invisible after a click on the field's label (#867)",
  { tag: "@core" },
  async ({ page }) => {
    // Why: while the list is open a press outside the field hides it but keeps its space, so a
    // click on the button below still lands. A click on the field's own label hands the focus back
    // to the field; the list must not stay hidden then, because it would still take the keys and
    // Enter would silently replace the typed command with a suggestion the person cannot see.
    await page.setExtraHTTPHeaders(admin);
    for (const field of ["any-command", "copy-command"]) {
      await page.goto(app.url + "/policy/rules/new");
      await page
        .locator("#copy-device")
        .selectOption({ label: "plug-4m-http" });
      const input = page.locator("#" + field);
      await input.fill("Pow");
      const list = page.locator(`#${field} + .cmd-sug`);
      await expect(list).toBeVisible();
      await page.locator(`label[for="${field}"]`).click();
      await input.pressSequentially("e");
      // The state a person sees: a list that takes the keys is a list they can see.
      await expect(list).toBeVisible();
      await expect(input).toHaveAttribute("aria-expanded", "true");
      await expect(list).not.toHaveClass(/cmd-sug-held/);
    }
  },
);

// Enter in a field with no list open submits its form, which is correct and reloads the page. A test
// that wants to read the field after Enter keeps the page by refusing the submit.
async function refuseSubmit(page: Page) {
  await page.evaluate(() =>
    document.addEventListener("submit", (e) => e.preventDefault(), true),
  );
}

test(
  "a press that becomes a native drag does not leave an invisible suggestion list taking Enter (#869)",
  { tag: "@core" },
  async ({ page }) => {
    // Why: while the list is open a press outside the field hides it but keeps its space (#867), and
    // the hold ends on the next mouseup. A press that turns into a native drag (a link, selected text)
    // sends dragstart and dragend but no mouseup, so the hold never ended: the list stayed invisible,
    // and once the focus was back in the field Enter silently replaced the typed command with the
    // first suggestion the person could not see. The stale "pressed" flag also made the next
    // focusout hold the list again.
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width: 1280, height: 900 });
    for (const field of ["any-command", "copy-command"]) {
      await page.goto(app.url + "/policy/rules/new");
      await page
        .locator("#copy-device")
        .selectOption({ label: "plug-4m-http" });
      const input = page.locator("#" + field);
      await input.fill("Pow");
      const list = page.locator(`#${field} + .cmd-sug`);
      await expect(list).toBeVisible();

      // A press on a link takes the focus (the list is held), and the pointer then moves far enough
      // for the browser to start dragging the link.
      const link = page.locator("header a[href], nav a[href]").first();
      const at = (await link.boundingBox())!;
      await page.mouse.move(at.x + at.width / 2, at.y + at.height / 2);
      await page.mouse.down();
      await expect(list).toHaveClass(/cmd-sug-held/);
      await page.mouse.move(at.x + at.width / 2 + 120, at.y + 200, {
        steps: 12,
      });
      await page.mouse.up();

      // The press is over: the list is gone, not just invisible.
      await expect(list).toBeHidden();
      await expect(list).not.toHaveClass(/cmd-sug-held/);
      await expect(input).toHaveAttribute("aria-expanded", "false");
      await refuseSubmit(page);
      await input.focus();
      await input.press("Enter");
      await expect(input).toHaveValue("Pow");

      // The press did not leave "pressed" behind: leaving the field later closes the list for good
      // instead of holding it again.
      await input.pressSequentially("e");
      await expect(list).toBeVisible();
      await input.press("Tab");
      await expect(list).toBeHidden();
      await expect(list).not.toHaveClass(/cmd-sug-held/);
    }
  },
);

test(
  "a suggestion list held by a press that is still down takes no keys (#869)",
  { tag: "@core" },
  async ({ page }) => {
    // Why: the held list is invisible but still in the layout, so that a click on the button below
    // lands where the button was. A list the person cannot see must not act on ArrowDown or Enter:
    // Enter would replace the typed command with a suggestion they never saw.
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width: 1280, height: 900 });
    for (const field of ["any-command", "copy-command"]) {
      await page.goto(app.url + "/policy/rules/new");
      await page
        .locator("#copy-device")
        .selectOption({ label: "plug-4m-http" });
      const input = page.locator("#" + field);
      await input.fill("Pow");
      const list = page.locator(`#${field} + .cmd-sug`);
      await expect(list).toBeVisible();

      const heading = (await page
        .getByRole("heading", { level: 1 })
        .boundingBox())!;
      await page.mouse.move(heading.x + 4, heading.y + 4);
      await page.mouse.down();
      await expect(list).toHaveClass(/cmd-sug-held/);
      // The focus comes back to the field while the press is still down.
      await refuseSubmit(page);
      await input.focus();
      await input.press("ArrowDown");
      await input.press("Enter");
      await expect(input).toHaveValue("Pow");
      await page.mouse.up();
    }
  },
);

test("an operator sees the rule read-only and cannot open New rule", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new?template=poweron");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved&rule=r[a-z0-9]{2,15}$/);
  const id = savedRuleID(page.url());

  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy/rules/" + id);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Restore power after an outage",
  );
  await expect(page.getByRole("button", { name: "Check now" })).toBeVisible();
  for (const name of ["Save rule", "Duplicate", "Disable"]) {
    await expect(page.getByRole("button", { name })).toHaveCount(0);
  }
  await expect(page.locator("input[name='want']")).toHaveCount(0);
  await shot(page, "rule-page-operator");
  expect(
    (
      await page.request.get(app.url + "/policy/rules/new", {
        headers: operator,
      })
    ).status(),
  ).toBe(403);
});
