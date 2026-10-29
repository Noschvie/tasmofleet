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

const ruleID = (url: string) => new URL(url).pathname.split("/").pop()!;

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
  await expect(page).toHaveURL(/\/policy\/rules\/r[a-z0-9]{2,15}\?ok=saved$/);
  const first = ruleID(page.url().split("?")[0]);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Local timezone",
  );
  await expect(page.getByText("Rule saved")).toBeVisible();
  await expect(page.locator(".dev-tile")).toHaveCount(4);
  await shot(page, "rule-page");

  // a second rule through "Any command"
  await page.goto(app.url + "/policy/rules/new");
  await page.locator("#any-command").fill("LedState");
  await page.getByRole("button", { name: "Continue" }).first().click();
  await expect(page).toHaveURL(/\/policy\/rules\/new\?command=LedState$/);
  await page.locator("input[name='want']").fill("3");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\/rules\/r[a-z0-9]{2,15}\?ok=saved$/);
  const second = ruleID(page.url().split("?")[0]);
  expect(second).not.toBe(first);

  // reorder: the second rule goes first. The first rule's address still names the timezone rule.
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

  // editing the rule keeps its id and the page it lands on
  await page.goto(app.url + "/policy/rules/" + first);
  await page.locator("textarea[name='note']").fill("local time everywhere");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(
    new RegExp(`/policy/rules/${first}\\?ok=saved$`),
  );
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

test("an operator sees the rule read-only and cannot open New rule", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new?template=poweron");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\/rules\/r[a-z0-9]{2,15}\?ok=saved$/);
  const id = ruleID(page.url().split("?")[0]);

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
