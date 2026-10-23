# tasmofleet DS: conventions

## Where the truth lives

- `ds-bundle/styles.css` is the only source of tokens and component styles. It is generated:
  edit `tools/styles.src.css`, then run `python3 -I tools/build.py` (adds the OKLCH ramps, the
  dark tokens and the `.is-hover` / `.is-active` / `.is-focus` state hooks).
- Preview pages come from `tools/pages.py`; `ds-bundle/README.md` has the rules, the
  component table and the contrast results.
- Class names are the app's (`tasmofleet/internal/web/static/app.css`); keep them.

## Tokens

- Surfaces: `--color-bg`, `--color-surface`, `--color-surface-2`, `--color-overlay`.
- Ink and lines: `--color-text`, `--color-text-2`, `--color-rule` (2px), `--color-divider`
  (hairline), `--color-border` (controls), `--color-border-overlay`.
- Accent (sparingly): `--color-accent`, `--color-on-accent`, `--color-accent-soft`,
  `--color-link`, `--color-focus`, `--color-selected`.
- Status: `--color-success`, `--color-warning`, `--color-danger`, `--color-info`,
  `--color-neutral`, each with `-soft`, `-text`, `-border`. Red only for danger.
- Ramps: `--color-accent-100` … `--color-accent-900` (same for every role).
- Type: `--font-heading`, `--font-body`, `--fs-xs` 12, `--fs-sm` 13, `--fs-body` 14,
  `--fs-md` 16, `--fs-lg` 19, `--fs-h1` 26.
- Space `--space-1` … `--space-12` (4px steps); `--radius-md` is 0; `--rule-w` 2px.
- Size: `--control-h-sm` 28, `--control-h` 34 (both 40 on phones); `--shadow-lg`, `--backdrop`.
- Themes: same names in light and dark; `data-theme="light|dark"` on `<html>` overrides
  `prefers-color-scheme`.

## Class vocabulary

- Page: `.top`, `.mainnav`, `.navtop`, `.navgroup`, `.menu`, `.page-head`, `.actions`, `.crumb`,
  `.cols`, `.grid`.
- Actions: `.button` / `button` with `.primary`, `.danger`, `.ghost`, `.sm`, `.block`, `.icon`;
  `button.info` for (i) help. One primary per region; labels centered except `.block`.
- Content: `.card` (first `h2` sits in the 2px top rule), `.facts`, `.stats` / `.stat`,
  `.badge.tone-ok|warn|bad|info|neutral`, `.tag`, `.empty-state`.
- Alerts: `.notice` (success), `.notice.plain` (info), `.warning`, `.error`.
- Tables: `table.dense`, `td.sel` + `.hit`, `td.p1`, `td.p2`, `td.s`, `tr.is-selected`,
  `.sortlink` / `.sortmark`, `.row-actions` (≤ 2 buttons), `.rowmenu` / `.rowmenu-body`,
  `.toolbar`, `.bulkbar` / `.bulk-count`, `.pager`.
- Overlays: `.menu`, `.rowmenu-body`, `.tooltip`, `.popover`, `dialog.dlg` (`.dlg-head`,
  `.dlg-body`, `.dlg-foot`).
- Navigation: `.tabs` (`aria-current="page"`), `.jump` select on phones.
- Power: `.relay-btn` with `.on` / `.unk`. Folds: `details.fold`, `details.m-fold`, `.fold-h`.

## Build snippet

```html
<section class="card" aria-labelledby="fleet-h">
  <h2 id="fleet-h">Fleet health</h2>
  <ul class="stats">
    <li>
      <a class="stat tone-ok" href="/devices?health=online"
        ><span class="num">17</span
        ><span class="what" data-glyph="✓">Online</span></a
      >
    </li>
    <li>
      <a class="stat tone-bad" href="/devices?health=offline"
        ><span class="num">7</span
        ><span class="what" data-glyph="✕">Offline</span></a
      >
    </li>
  </ul>
  <p class="notice plain" role="status">
    6 of 27 devices run an older version.
  </p>
  <p class="actions">
    <a class="button primary" href="/updates/new">New transaction</a>
  </p>
</section>
```
