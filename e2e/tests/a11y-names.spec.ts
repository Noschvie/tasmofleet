import { DatabaseSync } from "node:sqlite";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { seedProposals } from "../support/seed";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#1074): the Devices list had 60 power switches that a screen reader announced as "Toggle relay 1,
// currently ON": a person who walks the Power column cannot tell the rows apart. The same held for "Details"
// on every audit row, "Add" and "Ignore" on every proposal, "Use as reference" on every template, and "Actions"
// on every row menu. A control that is repeated per row must carry its row in its accessible name.
//
// The check reads the accessibility tree Chrome builds (CDP Accessibility.getFullAXTree, through Playwright's own
// session), not axe-core: axe is not a dependency of e2e/ and a patch release does not add one. The tree is the
// one assistive technology gets: it includes names taken through aria-labelledby (also from hidden text) and
// the <summary> disclosures, which Playwright's ariaSnapshot() leaves out. It groups every interactive element
// of the page's main region by role and accessible name. Two elements may share a name only when they do the
// same thing: links count as the same only with the same address.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(240_000);
  env = await startDevenv(["-template-shapes"]);
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: tpl-nous-1");
  await seedProposals(app); // the dashboard's "Needs attention" lists them with Add and Ignore
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const insert = db.prepare("INSERT INTO audit_log (at, actor, action, target, detail, result, remote_addr) VALUES (?, 'adam', 'command.send', ?, '{\"cmd\":\"Status 2\"}', 'ok', '127.0.0.1')");
    for (let i = 1; i <= 4; i++) insert.run(Date.now() + 3_600_000 + i * 1000, `device:${i}`); // four rows with a Details button each
  } finally {
    db.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

interface Item {
  role: string;
  name: string;
  url: string;
  expanded?: boolean;
}

const ROLES = new Set(["button", "link", "switch", "checkbox", "radio", "textbox", "combobox", "searchbox", "spinbutton", "slider", "tab", "DisclosureTriangle"]);

interface AXNode {
  nodeId: string;
  ignored?: boolean;
  role?: { value: string };
  name?: { value: string };
  properties?: { name: string; value: { value: unknown } }[];
  childIds?: string[];
}

/** items lists the interactive elements inside the main region with their accessible names (and the address of a link). */
async function items(page: Page): Promise<Item[]> {
  const cdp = await page.context().newCDPSession(page);
  try {
    const { nodes } = (await cdp.send("Accessibility.getFullAXTree")) as { nodes: AXNode[] };
    const byId = new Map(nodes.map((n) => [n.nodeId, n]));
    const main = nodes.find((n) => !n.ignored && n.role?.value === "main");
    expect(main, "the page has a main region").toBeTruthy();
    const out: Item[] = [];
    const walk = (n: AXNode) => {
      const role = n.role?.value ?? "";
      if (!n.ignored && ROLES.has(role)) {
        const prop = (k: string) => n.properties?.find((p) => p.name === k)?.value.value;
        out.push({ role, name: n.name?.value ?? "", url: String(prop("url") ?? ""), expanded: prop("expanded") as boolean | undefined });
      }
      for (const id of n.childIds ?? []) {
        const c = byId.get(id);
        if (c) walk(c);
      }
    };
    walk(main!);
    expect(out.length, "the tree lists interactive elements").toBeGreaterThan(5);
    return out;
  } finally {
    await cdp.detach();
  }
}

/** duplicates are the names shared by two or more elements that do not do the same thing. */
function duplicates(list: Item[]): string[] {
  const seen = new Map<string, number>();
  for (const it of list) {
    const key = `${it.role} "${it.name}"${it.role === "link" ? ` -> ${it.url}` : ""}`;
    seen.set(key, (seen.get(key) ?? 0) + 1);
  }
  const out: string[] = [];
  // the same link twice is one action; anything else with one name twice is not
  for (const [key, n] of seen) if (n > 1 && !key.startsWith("link ")) out.push(`${key} x${n}`);
  // a link with one name and several addresses is several actions with one name
  const urls = new Map<string, Set<string>>();
  for (const it of list) if (it.role === "link") urls.set(it.name, (urls.get(it.name) ?? new Set()).add(it.url));
  for (const [name, set] of urls) if (set.size > 1) out.push(`link "${name}" -> ${set.size} different addresses`);
  return out;
}

for (const width of [1280, 375]) {
  test(`Devices at ${width} px: every power switch and row menu names its device`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/devices");
    await expect(page.locator("button.pwr:not([disabled]):visible").first()).toBeVisible({ timeout: 60_000 });
    const list = await items(page);
    expect(duplicates(list), "no two controls of the list share a name").toEqual([]);
    const switches = list.filter((i) => i.role === "switch");
    expect(switches.length, "the fleet has switches").toBeGreaterThan(10);
    // "<device name> relay N": not the bare "relay 1" the hidden text alone would give, and no state in the name
    for (const s of switches) expect(s.name, "a switch is named by its row").toMatch(/^\S.* relay \d$|^\S.* power$/);
    expect(switches.some((s) => /currently|^Toggle/.test(s.name)), "the state is not in the name").toBe(false);
    expect(list.filter((i) => i.name === "Actions"), "no trigger is named just Actions").toEqual([]);
    expect(list.filter((i) => i.role === "DisclosureTriangle" && i.name.startsWith("Actions for ")).length).toBeGreaterThan(10);
  });
}

test("Dashboard: Needs attention names the device behind Add and Ignore", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/");
  const list = await items(page);
  const names = list.filter((i) => i.role === "button").map((i) => i.name);
  expect(names, "a proposal is on the page").toContain("Add Kitchen");
  expect(names).toContain("Ignore Kitchen");
  expect(names.filter((n) => n === "Add" || n === "Ignore")).toEqual([]);
  expect(duplicates(list)).toEqual([]);
});

test("Audit: every Details button says which event it opens", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/audit?since=all");
  const list = await items(page);
  const details = list.filter((i) => i.role === "DisclosureTriangle" && i.name.startsWith("Details"));
  expect(details.length, "audit rows with a Details button").toBeGreaterThan(3);
  for (const d of details) expect(d.name).toMatch(/^Details of .+ at .+/);
  expect(duplicates(list)).toEqual([]);
  // the disclosure tells its state itself and controls the row that holds the content
  const first = page.locator(".audittable summary").first();
  await expect(first).toHaveAttribute("aria-controls", /^audit-detail-\d+$/);
  const target = await first.getAttribute("aria-controls");
  await expect(page.locator(`#${target}`)).toHaveCount(1);
  await first.click();
  expect((await items(page)).filter((i) => i.name.startsWith("Details of")).some((i) => i.expanded === true), "an open disclosure reports expanded").toBe(true);
});

test("Policy templates: every Use as reference names its device", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/templates");
  await page.getByRole("button", { name: "Read again" }).click();
  const tie = page.locator(".gt-card", { hasText: "tpl-tie-1" });
  await expect(tie.getByText("Choose a reference.")).toBeVisible({ timeout: 60_000 });
  const list = await items(page);
  const refs = list.filter((i) => i.name.startsWith("Use as reference"));
  expect(refs.length, "the tied group offers one link per device").toBeGreaterThanOrEqual(4);
  for (const r of refs) expect(r.name).toMatch(/^Use as reference \S+/);
  expect(duplicates(list)).toEqual([]);
});
