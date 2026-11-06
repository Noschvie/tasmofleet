// Changelog fragments (#527). Every change adds changes/unreleased/<issue>-<slug>.md:
//
//   ---
//   section: Fixed
//   ---
//   The line as it should read in CHANGELOG.md (#N, thanks @handle)
//
// The line text is the source of truth (credits, several issue refs); the file name only orders the lines
// (leading number, then name). Sections: Upgrade notes, Added, Changed, Fixed, Removed, Breaking, Security.
//
//   changelog-assemble.mjs assemble --version X.Y.Z [--name "Name"] [--date YYYY-MM-DD]
//       writes the "## X.Y.Z "Name" (date)" section into CHANGELOG.md and deletes the fragments
//   changelog-assemble.mjs check
//       validates every fragment
//   changelog-assemble.mjs gate --base <commit>
//       PR gate (--code false for docs-only PRs: validation only): a PR that changes code adds a fragment, or is the release commit (adds a new version section),
//       or says "changelog: skip - <reason>" in its body (env PR_BODY)
import { readFileSync, writeFileSync, readdirSync, existsSync, unlinkSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

export const SECTIONS = ["Upgrade notes", "Added", "Changed", "Fixed", "Removed", "Breaking", "Security"];
export const DIR = "changes/unreleased";
const NAME_RE = /^[a-z0-9][a-z0-9-]*\.md$/;

export function parseFragment(file, text) {
  if (!NAME_RE.test(file))
    throw new Error(`${file}: bad file name, use lowercase <issue>-<slug>.md`);
  if (text.startsWith("\uFEFF")) throw new Error(`${file}: starts with a byte order mark (BOM); save it as plain UTF-8`);
  const m = /^---\r?\n([\s\S]*?)\r?\n---\r?\n([\s\S]*)$/.exec(text);
  if (!m) throw new Error(`${file}: missing front matter: the file must start with "---", then "section: <name>", then "---", then the line`);
  let section;
  for (const l of m[1].split(/\r?\n/)) {
    const kv = /^([A-Za-z_-]+):[ \t]*(.*?)[ \t]*$/.exec(l);
    if (!kv) throw new Error(`${file}: front matter line "${l}" is not "key: value"`);
    if (kv[1] !== "section") throw new Error(`${file}: unknown front matter key "${kv[1]}" (only "section" is allowed)`);
    section = kv[2];
  }
  if (section === undefined) throw new Error(`${file}: front matter has no "section:" line`);
  if (!SECTIONS.includes(section))
    throw new Error(`${file}: unknown section "${section}" (allowed: ${SECTIONS.join(", ")})`);
  const body = m[2].trim();
  if (!body) throw new Error(`${file}: empty body`);
  if (/^[-*+][ \t]/.test(body))
    throw new Error(`${file}: the body must not start with a bullet marker ("- "); the assembler adds it`);
  return { file, section, body };
}

export function readFragments(dir = DIR) {
  const entries = existsSync(dir) ? readdirSync(dir, { withFileTypes: true }) : [];
  const errors = [];
  for (const e of entries) {
    if (e.isDirectory()) errors.push(`${dir}/${e.name}/: subdirectories are not read; put the fragment directly in ${dir}/`);
    else if (!e.name.endsWith(".md") && e.name !== ".gitkeep") errors.push(`${dir}/${e.name}: only .md fragments belong here`);
  }
  const names = entries.filter((e) => e.isFile() && e.name.endsWith(".md")).map((e) => e.name);
  const frags = [];
  for (const n of names) {
    try {
      frags.push(parseFragment(n, readFileSync(join(dir, n), "utf8")));
    } catch (e) {
      errors.push(e.message);
    }
  }
  if (errors.length) throw new Error(errors.join("\n"));
  const num = (f) => (/^\d+/.test(f) ? Number(/^\d+/.exec(f)[0]) : Infinity);
  frags.sort((a, b) => num(a.file) - num(b.file) || (a.file < b.file ? -1 : 1));
  return frags;
}

const bullet = (body) => "- " + body.split(/\r?\n/).map((l, i) => (i && l ? "  " + l.trim() : l.trim())).join("\n");

export function assembleSection(frags, { version, name, date }) {
  let out = `## ${version}${name ? ` "${name}"` : ""} (${date})\n\n`;
  for (const s of SECTIONS) {
    const lines = frags.filter((f) => f.section === s);
    if (lines.length) out += `### ${s}\n\n${lines.map((f) => bullet(f.body)).join("\n")}\n\n`;
  }
  return out;
}

// -> the new CHANGELOG text; throws on anything that would lose or misplace a line.
export function insertSection(changelog, section, version) {
  if (/^## Unreleased\b/m.test(changelog))
    throw new Error('CHANGELOG.md still has a "## Unreleased" section: move its lines into fragments first');
  const heading = /^## \d+\.\d+\.\d+/m;
  if (changelog.split("\n").some((l) => l === `## ${version}` || l.startsWith(`## ${version} `)))
    throw new Error(`CHANGELOG.md already has a section for ${version}`);
  const m = heading.exec(changelog);
  const at = m ? m.index : changelog.length;
  const head = changelog.slice(0, at);
  return (m ? head : head.replace(/\n*$/, "\n\n")) + section.replace(/\n*$/, m ? "\n\n" : "\n") + changelog.slice(at);
}

function nameFor(changelog, version) {
  const minor = version.split(".").slice(0, 2).join(".") + ".";
  for (const l of changelog.split("\n")) {
    const m = /^## (\d+\.\d+\.\d+) "([^"]+)"/.exec(l);
    if (m && m[1].startsWith(minor)) return m[2];
  }
  return "";
}

const cmp = (a, b) => {
  const x = a.split(".").map(Number), y = b.split(".").map(Number);
  return x[0] - y[0] || x[1] - y[1] || x[2] - y[2];
};
const plainVersions = (t) => [...t.matchAll(/^## (\d+\.\d+\.\d+)(?= |$)/gm)].map((m) => m[1]);

// Today in the maintainer's time zone (releases are dated by the local day, not UTC).
const today = () => new Date().toLocaleDateString("sv-SE", { timeZone: "Europe/Berlin" });

export function checkRelease(changelog, { version, name, date }) {
  if (!/^\d+\.\d+\.\d+$/.test(version ?? ""))
    throw new Error("usage: assemble --version X.Y.Z [--name NAME] [--date YYYY-MM-DD]");
  const have = plainVersions(changelog);
  if (have.includes(version)) throw new Error(`CHANGELOG.md already has a section for ${version}`);
  if (have.length && have.every((v) => cmp(version, v) < 0))
    throw new Error(`${version} is older than the newest section (${have.reduce((a, b) => (cmp(a, b) >= 0 ? a : b))}); check the version`);
  if (have.some((v) => cmp(version, v) < 0))
    throw new Error(`${version} is older than a section already in CHANGELOG.md; check the version`);
  const [x, y, z] = version.split(".");
  if (z !== "0" && !have.some((v) => v.startsWith(`${x}.${y}.`)))
    throw new Error(`${version} is a patch but CHANGELOG.md has no ${x}.${y}.0 section: for a new minor use ${x}.${y}.0`);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date) || new Date(`${date}T00:00:00Z`).toISOString().slice(0, 10) !== date)
    throw new Error(`DATE "${date}" is not a real date in the form YYYY-MM-DD`);
  if (/["\r\n]/.test(name ?? "")) throw new Error('NAME must not contain double quotes or line breaks');
}

function assemble(opts) {
  const changelog = readFileSync("CHANGELOG.md", "utf8");
  const frags = readFragments();
  if (!frags.length) throw new Error(`no fragments in ${DIR}/: nothing to assemble`);
  const name = opts.name || nameFor(changelog, opts.version ?? "");
  const date = opts.date || today();
  checkRelease(changelog, { version: opts.version, name, date });
  if (!name && opts.version.split(".")[2] === "0")
    throw new Error("NAME is required for a new minor release (the next free name in docs/release-names.md)");
  const next = insertSection(changelog, assembleSection(frags, { version: opts.version, name, date }), opts.version);
  writeFileSync("CHANGELOG.md", next);
  for (const f of frags) unlinkSync(join(DIR, f.file));
  console.log(`assembled ${frags.length} fragment(s) into ## ${opts.version}${name ? ` "${name}"` : ""} (${date}), fragments deleted`);
}

export function optOut(body) {
  let b = body ?? "";
  for (let prev = ""; prev !== b; ) { prev = b; b = b.replace(/<!--[\s\S]*?-->/g, ""); } // until stable: no "<!--" survives
  const m = /^[ \t]*changelog:[ \t]*skip\b[ \t]*[-:,(]*[ \t]*(.*?)[ \t]*\)?[ \t]*$/im.exec(b);
  return m && m[1] ? m[1] : null;
}

const git = (...a) => execFileSync("git", a, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
const versions = (t) => [...t.matchAll(/^## (\d+\.\d+\.\d+[^ ]*)/gm)].map((m) => m[1]);

// code: the PR changes more than docs. Validation (fragments, no "Unreleased", no leftovers in a release
// commit) runs for every PR, so a broken fragment cannot reach main; only "must add a fragment" needs code.
function gate(base, code) {
  const frags = readFragments(); // every fragment in the tree must be valid, whatever else the PR does
  const changelog = readFileSync("CHANGELOG.md", "utf8");
  if (/^## Unreleased\b/m.test(changelog)) {
    console.log('changelog check FAILED: CHANGELOG.md has a "## Unreleased" section. Lines no longer go there: add a fragment (changes/README.md).');
    return 1;
  }
  const old = versions(git("show", `${base}:CHANGELOG.md`));
  const fresh = versions(changelog).filter((v) => !old.includes(v));
  if (fresh.length) {
    if (frags.length) {
      console.log(`changelog check FAILED: new section ${fresh.join(", ")} (release commit) but ${frags.length} fragment(s) are left in ${DIR}/: ${frags.map((f) => f.file).join(", ")}. Run make changelog-assemble, which deletes them.`);
      return 1;
    }
    return console.log(`changelog: release commit, new section ${fresh.join(", ")}`), 0;
  }
  if (!code) return console.log("changelog: docs only, fragments valid"), 0;
  const added = git("diff", "--diff-filter=A", "--name-only", `${base}...HEAD`)
    .split("\n")
    .filter((f) => f.startsWith(`${DIR}/`) && f.endsWith(".md"));
  if (added.length) return console.log(`changelog: fragment added (${added.join(", ")})`), 0;
  if (process.env.PR_AUTHOR === "renovate[bot]") return console.log("changelog: dependency update by renovate[bot], no fragment needed"), 0;
  const why = optOut(process.env.PR_BODY);
  if (why && why.includes("<reason>")) {
    console.log("changelog check FAILED: the skip marker still has the placeholder <reason>; write the actual reason.");
    return 1;
  }
  if (why) return console.log(`changelog: skipped, reason: ${why}`), 0;
  console.log(
    `changelog check FAILED: this PR changes code but adds no changelog fragment.\n` +
      `Add ${DIR}/<issue>-<slug>.md:\n\n---\nsection: Fixed\n---\nWhat changed, for users (#N, thanks @handle for outside reports)\n\n` +
      `(section: ${SECTIONS.join(", ")}). Do NOT edit CHANGELOG.md; the release commit assembles the fragments.\n` +
      `Nothing user-visible (dependency bump, CI only)? Put 'changelog: skip - <reason>' on a line of the PR body and re-run the job.`,
  );
  return 1;
}

function main(argv) {
  const [cmd, ...rest] = argv;
  const arg = (n) => (rest.includes(n) ? rest[rest.indexOf(n) + 1] : undefined);
  if (cmd === "assemble") return assemble({ version: arg("--version"), name: arg("--name"), date: arg("--date") }), 0;
  if (cmd === "check") return console.log(`${readFragments().length} fragment(s) ok`), 0;
  if (cmd === "gate") {
    if (!arg("--base")) throw new Error("usage: gate --base <commit>");
    return gate(arg("--base"), arg("--code") !== "false");
  }
  console.error("usage: changelog-assemble.mjs assemble|check|gate ...");
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`changelog: ${e.message}`);
    process.exit(1);
  }
}
