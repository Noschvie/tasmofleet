# Design prototype and tools

**What binds the app:** the admin page `/styleguide` in the running app (built from the real
partials) and `docs/spec/design-system.md`. The app does not read this folder at build time.

| Path | What |
| --- | --- |
| `v2/` | The Claude Design prototype the UI was ported from (design v2): `template.html`, the reference manifest for the pixel comparison (`make visual`); see `v2/README.md` |
| `tools/v2-fixture.mjs` | Generates `testdata/design/v2-fixture.json`, the fleet the devenv shows with `-design-fixture` (checked by `e2e/tests/visual-tool.spec.ts`) |
| `tools/brand-icons.sh` | Regenerates the raster icons from `assets/branding/logo.svg` |

The v1 design system (the `ds-bundle` upload root, its build scripts and the Claude Design sync
notes) was removed on 2026-10-30; it is in the git history before that date.
