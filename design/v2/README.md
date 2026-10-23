# Design v2: the second Claude Design prototype

Binding source of the design v2 port. Copied from the design export and not edited.

| Path | What |
| --- | --- |
| `template.html` | The Claude Design `x-dc` component: markup, tokens, logic and mock data (read as data, never executed by the app) |
| (not in the repository) `artifact-*.html` + `assets/` | The self-contained prototype bundle (fonts, React and the Claude Design runtime) is kept outside the repository for licence reasons. `make visual` renders it for the tool's self-test; point `PROTO_BUNDLE_DIR` at its directory |
| `ref-manifest.sha256` | SHA-256 of the 1312 reference screenshots (render v6: widths 1280, 1440, 2080 for the main screens, 375; two logo crops) (`sha256sum` format). The PNGs (~150 MB) stay outside the repository; a run fails when a reference differs from its pinned hash |

## Pixel comparison

```sh
make visual                                  # self-test, then every reference; at most 2 pages at a time
make visual SCREENS=P2,device-console        # entries whose owner or id contains a word
make visual PROTO_REF_DIR=/path/to/ref       # default: the design export's v2/ref
```

Output: `e2e/test-results/visual/report.html` (per package and per reference, with links to the actual,
expected and diff images). The entry table is `e2e/support/visual-entries.ts`, the tool
`e2e/support/visual.ts`, the app instances `e2e/support/visual-stack.ts`.

## Re-pinning the references

If the prototype is re-rendered (`v2/tools/run-all.mjs` in the backup), regenerate the manifest in
the same change, from the repository root:
`(cd <ref dir> && ls *.png | LC_ALL=C sort | xargs sha256sum) > design/v2/ref-manifest.sha256`.
New ids need an entry in `visual-entries.ts` (a test fails until they have one).

## Design fixture

`testdata/design/v2-fixture.json` is generated from `template.html` by `node design/tools/v2-fixture.mjs`
(it runs the prototype's own `makeDevices()`, rules, backups, users and scan data in a Node `vm`);
`node design/tools/v2-fixture.mjs --check` fails when the file is stale (an e2e test runs it).
