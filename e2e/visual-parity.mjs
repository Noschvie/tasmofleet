// Compares the screenshot sets written by tests/visual-parity.spec.ts with tolerance 0:
//   node visual-parity.mjs <base> <base again> <new> [diff dir]
// The app invents values per run (simulator ports, cache and rows ordered by time), so a page is
// only comparable when the base renders it identically twice ("stable"). The new set must equal
// the base on every stable page. Unstable pages are listed with the pixels the new set differs by,
// but do not fail the run. Exit 1 on a differing stable page, a missing page or a size change.
import { readdirSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import pixelmatch from "pixelmatch";
import { PNG } from "pngjs";

const [base, again, head, diffDir] = process.argv.slice(2);
if (!base || !again || !head) {
  console.error("usage: node visual-parity.mjs <base dir> <base again dir> <new dir> [diff dir]");
  process.exit(2);
}
const read = (dir, name) => PNG.sync.read(readFileSync(join(dir, name)));
const count = (a, b, out) => pixelmatch(a.data, b.data, out?.data, a.width, a.height, { threshold: 0, includeAA: true });
const names = new Set([base, again, head].flatMap((d) => readdirSync(d)).filter((f) => f.endsWith(".png")));
let stable = 0;
let unstable = 0;
let bad = 0;
for (const name of [...names].sort()) {
  let a, a2, b;
  try {
    [a, a2, b] = [read(base, name), read(again, name), read(head, name)];
  } catch {
    console.log(`MISSING  ${name}`);
    bad++;
    continue;
  }
  if (a.width !== b.width || a.height !== b.height) {
    const sameTwice = a.width === a2.width && a.height === a2.height;
    console.log(`SIZE     ${name}: ${a.width}x${a.height} vs ${b.width}x${b.height}${sameTwice ? "" : " (base is unstable too)"}`);
    if (sameTwice) bad++;
    else unstable++;
    continue;
  }
  const noise = a2.width === a.width && a2.height === a.height ? count(a, a2) : -1;
  const diff = new PNG({ width: a.width, height: a.height });
  const n = count(a, b, diff);
  if (noise === 0) {
    stable++;
    if (n > 0) {
      console.log(`DIFF     ${name}: ${n} of ${a.width * a.height} pixels (${((100 * n) / (a.width * a.height)).toFixed(4)} %)`);
      if (diffDir) {
        mkdirSync(diffDir, { recursive: true });
        writeFileSync(join(diffDir, name), PNG.sync.write(diff));
      }
      bad++;
    }
  } else {
    unstable++;
    console.log(`unstable ${name}: base differs from itself by ${noise} px, new differs from base by ${n} px`);
  }
}
console.log(`parity: ${stable} stable pages compared, ${bad} differ; ${unstable} unstable pages (base differs from itself) not decided by pixels`);
process.exit(bad ? 1 : 0);
