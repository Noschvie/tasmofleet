import { defineConfig, devices } from "@playwright/test";

// Every spec starts the real binary (bin/tasmofleet, built by `make build`)
// and, where it needs devices or an IdP, bin/tasmofleet-devenv, through
// support/stack.ts. Ports and data directories are per spec, so nothing is
// shared between specs, and spec files run in parallel workers
// (E2E_WORKERS, default 6; CI sets a lower value for its runner).
//
// Three projects: "chromium" is the behaviour suite CI runs (make e2e); "visual" is the pixel
// comparison against the design prototype (make visual, not part of CI, plan decision D7); "docs"
// writes the style guide's screenshots into docs/design/styleguide (make styleguide-docs, D18). The
// visual and docs projects render with the flags the references were made with (design/v2/ref HOWTO.md).
const visualSpecs = /visual-(v2|selftest|parity)\.spec\.ts$/;
const docsSpecs = /styleguide-docs\.spec\.ts$/;
const renderLikeTheReferences = {
  ...devices["Desktop Chrome"],
  deviceScaleFactor: 1,
  reducedMotion: "reduce" as const,
  launchOptions: { args: ["--force-color-profile=srgb", "--font-render-hinting=none"] },
};

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  retries: 0,
  workers: Number(process.env.E2E_WORKERS ?? 6),
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["list"]] : [["list"], ["html", { open: "never" }]],
  use: { trace: "retain-on-failure" },
  // make visual starts the fleets once for all workers (a failed comparison restarts a worker)
  globalSetup: process.env.VISUAL_STACKS ? "./support/visual-global.ts" : undefined,
  projects: [
    { name: "chromium", testIgnore: [visualSpecs, docsSpecs], use: { ...devices["Desktop Chrome"] } },
    { name: "visual", testMatch: visualSpecs, timeout: 120_000, use: renderLikeTheReferences },
    { name: "docs", testMatch: docsSpecs, timeout: 120_000, use: renderLikeTheReferences },
  ],
});
