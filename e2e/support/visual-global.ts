import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { chromium } from "@playwright/test";
import { entries } from "./visual-entries";
import { selectedEntries, stackKey, stacksFile } from "./visual-select";
import { startVisualStack, type VisualStack } from "./visual-stack";

// Starts the app instances the selected entries need, once for the whole run, and writes their
// addresses to stacks.json for the workers. They must not live in a worker: Playwright restarts a
// worker after every failed test, and a failing comparison is the normal result until the port is
// done, so a per-worker stack would be rebuilt (about 5 minutes of seeding) after every red screen.
// Only active for `make visual` (VISUAL_STACKS=1), never in the CI project.
export default async function globalSetup(): Promise<(() => Promise<void>) | undefined> {
  if (!process.env.VISUAL_STACKS) return undefined;
  // a parity run (tests/visual-parity.spec.ts) renders only the entries that need no clicks
  const needed = selectedEntries(entries).filter((e) => !process.env.PARITY_OUT || ((e.steps?.length ?? 0) === 0 && !e.mutates));
  const keys = [...new Set(needed.map(stackKey))].sort();
  const browser = await chromium.launch({ args: ["--force-color-profile=srgb", "--font-render-hinting=none"] });
  const started: VisualStack[] = [];
  const out: Record<string, { url: string; placeholders: Record<string, string> }> = {};
  const stopAll = async () => {
    await Promise.allSettled(started.map((s) => s.stop()));
    await browser.close();
  };
  try {
    // one after the other: each fleet is 60 simulators, and the desktop is shared
    for (const key of keys) {
      const [kind] = key.split("+");
      const s = await startVisualStack(kind as VisualStack["kind"], browser, key.endsWith("+mutating"));
      started.push(s);
      out[key] = { url: s.app.url, placeholders: Object.fromEntries(s.placeholders) };
      console.log(`visual: stack ${key} ready at ${s.app.url}`);
    }
    mkdirSync(dirname(stacksFile), { recursive: true });
    writeFileSync(stacksFile, JSON.stringify(out, null, 1));
  } catch (e) {
    await stopAll();
    throw e;
  }
  return stopAll;
}
