// The pages the card-width guard (card-widths.spec.ts) and the tablet guards (tablet-guards.spec.ts)
// measure, as [name, path]; the path is a function where it depends on the seeded fleet.
export interface PageContext {
  lamp: () => string; // device path of the seeded "lamp-1m"
  txn: () => string; // path of a transaction detail
  fwVersion: () => string;
}

export function pageList(ctx: PageContext): [string, () => string][] {
  const tabs = ["general", "network", "mqtt", "discovery", "notifications", "firmware", "signin", "advanced"];
  // [name, path]: path is a function where it depends on the seeded fleet
  const pages: [string, () => string][] = [
    ["dashboard", () => "/"],
    ["devices list", () => "/devices"],
    ["device overview", () => ctx.lamp()],
    ...["settings", "console", "backups", "replace", "edit", "history"].map((t): [string, () => string] => [`device ${t}`, () => `${ctx.lamp()}/${t}`]),
    ["add devices", () => "/devices/new"],
    ["policy overview", () => "/policy"],
    ["policy rule new", () => "/policy/rules/new"],
    ["policy history", () => "/policy/history"],
    ["policy io", () => "/policy/io"],
    ["policy rule", () => "/policy/rules/r1"],
    ["policy templates", () => "/policy/templates"],
    ["policy deviations", () => "/policy/deviations"],
    ["updates new", () => "/updates/new"],
    ["transactions", () => "/transactions"],
    ["transaction detail", () => ctx.txn()],
    ["firmware", () => "/firmware"],
    ["firmware release", () => `/firmware/releases/${ctx.fwVersion()}`],
    ...tabs.map((t): [string, () => string] => [`system settings ${t}`, () => `/settings/system?tab=${t}`]),
    ["system snapshots", () => "/settings/system/snapshots"],
    ["devices backups", () => "/devices/backups"],
    ["audit", () => "/audit?since=all"],
    ["users", () => "/users"],
    ["about", () => "/about"],
    ["whats new", () => "/whats-new"],
  ];
  return pages;
}
