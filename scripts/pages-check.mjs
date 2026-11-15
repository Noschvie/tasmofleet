// Guards of the website's pictures (#859). The website's screenshots and update animation are build output of
// `make pages` (the demo-media run), never committed copies: a copy only changes when somebody remembers to renew
// it, and the project page showed the UI of 0.28.0 next to a demo of 0.37.0.
//
//   node scripts/pages-check.mjs --source [site]   fails when the website's source tree holds a picture
//   node scripts/pages-check.mjs <pagesDir>        fails when index.html links a file the Pages artifact lacks
//                                                  or links an empty one (a failed run must not publish a broken page)
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, extname } from "node:path";

// Fonts, wordmarks and the favicon are source files of the website; everything raster is generated.
const pictures = new Set([".png", ".webp", ".jpg", ".jpeg", ".gif", ".avif"]);

/** committedPictures lists the raster pictures under dir (relative paths). */
export function committedPictures(dir) {
  const found = [];
  const walk = (d) => {
    for (const e of readdirSync(d, { withFileTypes: true })) {
      const p = join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (pictures.has(extname(e.name).toLowerCase())) found.push(relative(dir, p));
    }
  };
  walk(dir);
  return found.sort();
}

/** missingLinks lists the local files index.html of dir links (src and href) that are absent or empty. */
export function missingLinks(dir) {
  const html = readFileSync(join(dir, "index.html"), "utf8");
  const bad = [];
  for (const m of html.matchAll(/\b(?:src|href)="([^"]*)"/g)) {
    const link = m[1].split(/[?#]/)[0];
    if (link === "" || link === "./" || /^([a-z][a-z0-9+.-]*:|\/\/)/i.test(link)) continue;
    const file = join(dir, link);
    if (!existsSync(file)) bad.push(`${link} (missing)`);
    else if (statSync(file).isFile() && statSync(file).size === 0) bad.push(`${link} (empty)`);
  }
  return [...new Set(bad)];
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  const args = process.argv.slice(2);
  let failures;
  if (args[0] === "--source") {
    const dir = args[1] ?? "site";
    failures = committedPictures(dir).map((f) => `${dir}/${f}: a committed picture; make pages generates them`);
  } else if (args.length === 1) {
    failures = missingLinks(args[0]).map((f) => `${args[0]}/index.html links ${f}`);
  } else {
    console.error("usage: pages-check.mjs --source [site] | <pagesDir>");
    process.exit(2);
  }
  for (const f of failures) console.error(`pages-check: ${f}`);
  process.exit(failures.length ? 1 : 0);
}
