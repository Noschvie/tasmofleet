// Generates testdata/design/v2-fixture.json from the Claude Design prototype.
//
// The prototype builds its fleet with makeDevices() (seeded rng(11), so it is
// deterministic) and keeps rules, backups, users and scan results as literals.
// This script runs exactly that code, unchanged, in a Node vm sandbox and writes
// the data the design fixture of tasmofleet-devenv and the e2e seed reproduce
// (design/v2/README.md). Nothing is copied by
// hand, so the fixture cannot drift from design/v2/template.html.
//
//   node design/tools/v2-fixture.mjs            # rewrites testdata/design/v2-fixture.json
//   node design/tools/v2-fixture.mjs --check    # exits 1 when the file is out of date
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const template = join(here, "../v2/template.html");
const out = join(here, "../../testdata/design/v2-fixture.json");

const html = readFileSync(template, "utf8");
const start = html.indexOf('<script type="text/x-dc" data-dc-script');
const body = html.slice(
  html.indexOf(">", start) + 1,
  html.indexOf("</script>", start),
);
// The data section runs from the first constant to the component class.
const from = body.indexOf("const LATEST=");
const to = body.indexOf("class Component extends DCLogic");
if (start < 0 || from < 0 || to < 0)
  throw new Error("template.html: data section markers not found");

// U0 (the users), P0 (discovery proposals) and M0 (address changes) live inside the component
// class: cut out their array literals.
function literal(name) {
  const at = body.indexOf(`const ${name}=[`);
  if (at < 0) throw new Error(`template.html: const ${name} not found`);
  let depth = 0;
  let quote = "";
  for (let i = body.indexOf("[", at); i < body.length; i++) {
    const c = body[i];
    if (quote) {
      if (c === "\\") i++;
      else if (c === quote) quote = "";
    } else if (c === "'" || c === '"' || c === "`") quote = c;
    else if (c === "[") depth++;
    else if (c === "]" && --depth === 0) return `const ${name}=${body.slice(body.indexOf("[", at), i + 1)};`;
  }
  throw new Error(`template.html: const ${name} is not terminated`);
}

const sandbox = vm.createContext({});
vm.runInContext(
  body.slice(from, to) +
    literal("U0") +
    literal("P0") +
    literal("M0") +
    "\nthis.__fx = {LATEST, RELEASES, OLDER, RULES, BACKUPS, SCAN, U0, P0, M0, devices: makeDevices()};\nthis.__fx.txn = makeTxn(JSON.parse(JSON.stringify(this.__fx.devices)));",
  sandbox,
  { filename: "template.html (data section)" },
);
const fx = sandbox.__fx;

// Only what the app can reproduce or the seed needs. Addresses, MACs, uptime,
// RSSI, watts and relative times are not carried over: they are masked in the
// comparison (plan 5.2).
const data = {
  source:
    "design/v2/template.html (makeDevices, RELEASES, RULES, BACKUPS, U0, SCAN, P0, M0, makeTxn)",
  latest: fx.LATEST,
  releases: [...fx.RELEASES.map((r) => r.v), ...fx.OLDER],
  // the three newest releases with the date and notes the firmware page shows (P5)
  releaseNotes: fx.RELEASES.map((r) => ({
    version: r.v,
    date: r.date,
    notes: r.notes,
  })),
  devices: fx.devices.map((d) => ({
    id: d.id,
    name: d.name,
    kind: d.kind,
    chip: d.chip,
    firmware: d.fw,
    status: d.status,
    power: d.power,
    relays: d.kind === "Switch" ? 2 : 1,
    telePeriod: d.tele,
    powerOnState: d.pos,
    mqttHost: d.mqttHost,
  })),
  rules: fx.RULES,
  backups: fx.BACKUPS,
  users: fx.U0,
  scan: fx.SCAN,
  // P0: the discovery proposals ("new", or "ignored" = dismissed); M0: known devices that
  // announced a new address.
  proposals: fx.P0.map(([id, name, host, ip, mac, , , src, , , st]) => ({
    id,
    name,
    host,
    ip,
    mac: mac.toLowerCase(),
    via: src === "MQTT discovery" ? "mqtt" : "scan",
    status: st,
  })),
  moves: fx.M0.map((m) => ({
    id: m.id,
    device: m.devId,
    address: m.new,
    via: /MQTT/i.test(m.seen) ? "mqtt" : "scan",
  })),
  transaction: {
    id: fx.txn.id,
    release: fx.txn.release,
    selected: fx.txn.sel,
    devices: fx.txn.devices.map((t) => ({
      id: t.id,
      name: t.name,
      from: t.from,
      state: t.st,
    })),
  },
};
const text = JSON.stringify(data, null, 1) + "\n";

if (process.argv.includes("--check")) {
  let have = "";
  try {
    have = readFileSync(out, "utf8");
  } catch {
    /* missing counts as out of date */
  }
  if (have !== text) {
    console.error(
      "testdata/design/v2-fixture.json is out of date: run node design/tools/v2-fixture.mjs",
    );
    process.exit(1);
  }
} else {
  writeFileSync(out, text);
  console.log(
    `wrote ${out}: ${data.devices.length} devices, ${data.rules.length} rules, ${data.backups.length} backups`,
  );
}
