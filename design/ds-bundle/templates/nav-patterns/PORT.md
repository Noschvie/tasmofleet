# Port guide: nav-patterns → tasmofleet app

The prototype pages are the output of the app's own templates (`internal/web/templates/*.html`,
`layout.html`, `partials.html`, the Columns menu from `tableprefs.go`) with sample data, plus the
changes below. The app's scripts run unchanged in the prototype (`static/select.js`, `jump.js`,
`dialog.js`), except `menu.js`, which has one changed line. Porting means three things:

1. **CSS**: the app loads `styles.css` (DS, same class names as `app.css`) and then `patterns.css`.
   Every rule in `patterns.css` uses existing DS tokens only.
2. **Markup**: the template diffs below. Class names are the app's. New names are listed at the
   top of `patterns.css`.
3. **Server/JS**: the items marked **server** or **js**. Scope: **now** = device delete confirmation page, proxy-header account kind (display only), device Restart, bulk Send command. **later** = bulk Back up, bulk Restart, Scan / Discovered split; the design shows them, the templates add them when the server has them.

## CSS

| In the prototype                                    | Port                                                                                                                                                                                                                                                                                                                                                         |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `patterns.css`, NEW block                           | New classes. Paste into `styles.css` (the code repo's source of it).                                                                                                                                                                                                                                                                                         |
| `patterns.css`, "app classes missing in styles.css" | `td.rowacts`, `.colmenu`/`.colbtn`/`.colpanel`/`.colrow`/`.colhint`/`.collabel`/`.colmoves`, `.tabletools`, `.tablewrap`, `td .cell > .dev`, `pre.log`, `.chips(.scroll)`. They exist in `app.css` but not in `styles.css`. Port them here in the DS look. **Rename `td.rowact` → `td.rowacts` in `styles.css`** (2 rules + the `:not(.rowact)` label rule). |
| `app.css` classes made obsolete                     | `.subnav`, `.jumpnav`, `form.setform > .jumpnav`, `.rel-older-toggle`, `#rel-older` rules, `.reltable td.rel-menu`, `.rel-wide`, `tr.rel-old`, `.dev-head .actions` phone stretch.                                                                                                                                                                           |
| Breakpoints                                         | Phone = **48rem** everywhere (`styles.css`, `app.css`, `patterns.css`). `patterns.css` adds 60rem (bulk bar actions fold into one menu) and 72rem (Columns chooser, like `tables.js`).                                                                                                                                                                       |

## js: `static/menu.js` (one line)

```diff
-  function groups() { return document.querySelectorAll("nav.mainnav details, .user details"); }
+  function groups() { return document.querySelectorAll("nav.mainnav details, .user details, details.navpanel"); }
```

The phone menu is `details.navgroup.navpanel`. With this line it closes on Escape and on an outside
click, like every other menu. Without JS it opens and closes as a plain `<details>`.

## layout.html: top bar

```diff
-<nav aria-label="Main" class="mainnav">
+<nav aria-label="Main" class="mainnav d-only">
 …Devices / Updates / System groups unchanged, except:
-{{if .Features.Updates}}<a …/updates/new…>New transaction</a>
-<a …/transactions…>Transactions</a>{{end}}
+{{if .Features.Updates}}<a …/transactions…>Transactions</a>
+<a …/updates/new…>New transaction</a>{{end}}                       (#75: list page first)
-<div class="user">
+<div class="user d-only">
 <details class="navgroup"><summary class="who">…</summary>
 <div class="menu right">
 {{if eq .User.Method "local"}}<a href="{{.Base}}/account/password">Change password</a>{{else}}<span class="menu-note">Signed in through your identity provider</span>{{end}}
+<hr><form method="post" action="{{.Base}}/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Sign out</button></form>
 </div></details>
-<form method="post" action="{{.Base}}/logout">…<button type="submit" class="sm">Sign out</button></form>
 </div>
+{{template "navpanel" .}}
```

**server (now)**, #74: proxy-header accounts get `<span class="who">{{.User.Name}} <small>({{.RoleName}})</small></span>`
with no menu, and Sign out stays a plain button. This needs a third value of `.User.Method` (e.g.
`"header"`) in the template data.

New partial `navpanel`: the same links as `.mainnav`, grouped. Each group is a
`div.navpanel-sec` (`is-active` = the current group), headed by `p.navpanel-label`:

```html
<details class="navgroup navpanel m-only"><summary>Menu</summary>
<div class="navpanel-body"><nav aria-label="Main">
<div class="navpanel-sec{{if eq .Nav "dashboard"}} is-active{{end}}"><a href="{{.Base}}/"{{if eq .Nav "dashboard"}} aria-current="page"{{end}}>Dashboard</a></div>
<div class="navpanel-sec{{if (devices group)}} is-active{{end}}"><p class="navpanel-label">Devices</p><a …>Devices</a><a …>Add devices</a><a …>Policy</a></div>
… Updates, System (same conditions as .mainnav) …
<div class="navpanel-sec"><p class="navpanel-label">{{.User.Name}} ({{.RoleName}})</p>{{if eq .User.Method "local"}}<a …>Change password</a>{{else}}<p class="menu-note">Signed in through your identity provider</p>{{end}}<form …logout…><button type="submit">Sign out</button></form></div>
</nav></div></details>
```

Use one helper (`navGroup .Nav`) for the active-group conditions, so `.mainnav` and the panel can't drift apart.

## Page header (every page with a `p.crumb`)

```diff
-<p class="crumb"><a href="{{$.Base}}/devices">Devices</a></p></div>
+<nav class="crumb trail" aria-label="Breadcrumb"><a href="{{$.Base}}/devices">Devices</a><span aria-current="page">{{.Device.Name}}</span></nav></div>
```

The position inside `.page-head` stays the same (after `.actions`). On phones only the parent shows, as "← Devices".
Device sub-pages: `Devices › {{name}} › Settings`. Replace "Back to the device" and "Back to {{name}}" with the trail.

One primary action per page. Moved into `.page-head .actions`:

- firmware.html: the `form.toolbar` "Refresh index" moves into the header, `button.primary`.
- policy.html: unchanged ("Edit the rules").

## Tabs (#18): one component = `addtabs` markup

`div.jump[hidden] > select[data-jump]` + `nav.tabs`, exactly as `addtabs` already does. Apply it to:

- **device.html**: replaces the header buttons. Console / Settings move into tabs, Edit moves into the Identity card (`p#probe-result.actions`: Refresh identity + Edit link).
  ```html
  <div class="page-head dev-head">
    <h1>…</h1>
    <div class="actions">
      {{if .CanReplace}}<a class="button ghost" href="…/replace"
        >Replace device</a
      >{{end}}
    </div>
    <nav class="crumb trail" …></nav>
  </div>
  {{template "devtabs" devtabs $.Base .Device.ID "overview" (len
  .BackupData.Backups) $.Features}}
  ```
  `devtabs`: Overview · Settings · Console · Backups `<span class="count">N</span>`. A tab the device can't use (never answered, updating) renders `aria-disabled="true"` with a `title` reason, and its `<option>` is `disabled`.
  Use the same tabs on settings.html, console.html and backups_compare.html, with the matching tab current.
- **settings.html**: drop `nav.jumpnav`. The groups stay as cards in `.setgrid`.
- **system_settings.html**: `nav.subnav` → `nav.tabs`. The existing `.jump` stays. Sections: Network · MQTT · Firmware · Discovery · Backups · Sign-in · Advanced. "Test MQTT" becomes a button in the MQTT section, and "Set by the container" becomes a fold in Advanced.
- **addtabs**: Add manually · Scan · Discovered `<span class="count accent">N</span>` (Import from TasmoAdmin stays last when enabled). **later**: discovery.html is split into Scan and Discovered; until then the tabs stay "Scan and discovered · Add manually · Import from TasmoAdmin".

## Row actions (#21): `td.rowacts`, last cell, same place at every width

- At most 2 direct buttons in `td.rowacts > div.actions`. With more, use one menu:
  `td.rowacts > details.rowmenu > summary.button.sm "Actions" + div.rowmenu-body`.
  **server**: put `rowmenu up` on the last two rows of a table.
- **partials.html `device_rows`**: new last cell + `<col class="c-act">` + `<th><span class="sr-only">Actions</span></th>` in devices.html:
  ```html
  <td data-label="Actions" class="rowacts">
    <details class="rowmenu">
      <summary class="button sm">Actions</summary>
      <div class="rowmenu-body">
        <a href="…/console">Console</a><a href="…/settings">Settings</a
        ><button type="button" hx-post="…/backup">Back up now</button
        ><button type="button" hx-post="…/restart">Restart</button>
        <hr />
        <button
          type="submit"
          class="danger"
          formaction="…/delete"
          formmethod="get"
          formnovalidate
          data-dialog="dlg-del-{{ID}}"
        >
          Delete…
        </button>
      </div>
    </details>
  </td>
  ```
  **server (now)**: `POST /devices/{id}/restart`, `GET /devices/{id}/delete` (confirmation page, same layout as the dialog).
- **partials.html `backups`**: `td.rowacts > div.actions` (Download link + Restore + Delete, #92) becomes one menu: Download · Restore… · ── · Delete… (`button.danger[data-dialog]`). Use the same menu in backups_compare.html.
- **firmware.html releases**: drop `td.rel-wide`, `td.rel-menu`, `#rel-older` and the `label.rel-older-toggle`. Files and Coverage get `m-hide`. Admins get a `td.rowacts` menu (Changelog and fleet · Prefetch for my fleet · Start update… = link to `/updates/new?release=`). Operators get an empty `td.rowacts` (#37). Releases after the first three go into `<details class="fold"><summary>Older releases<span class="meta">N, none needed by this fleet</span></summary>` with the same table.
- **firmware.html cached files** (#29, user fix 8): drop the Release, Chip / variant and SHA-256 columns. File becomes `{{.Filename}} <small class="muted keep">{{.Release}} · {{.Family}} / {{.Variant}}</small>`. `td.s.end` "Action" becomes `td.rowacts`. Installed on and Needed by stay plain labelled cells, so on phones they are lines inside the row card. The `table-fit` wrapper stays.

## Links vs buttons, confirmations (#19 #23)

- Every action is a `button`. A destructive action opens `dialog.dlg` (dialog.js already opens any `[data-dialog]`). Without JS, the same button is a GET submit to the confirmation page:
  `<button type="submit" class="danger" formaction="…/delete" formmethod="get" data-dialog="dlg-…">Delete…</button>`
  This replaces every `hx-confirm` (device delete, backup delete, cancel transaction, cancel job) and the `a.button.danger` "Delete…" links in firmware.html.
- Dialog layout: `.dlg-head`, `.dlg-body` with `dl.facts`, then `form.dlg-foot` holding Cancel (`button[data-close]`) and the confirming button, which repeats the trigger's verb and tone ("Delete file", `danger`).
- Confirmation pages (firmware_confirm, restore_confirm, replace, backups_prune) use the same layout as a card: `section.card.confirm` with `dl.facts` (not a bare `dl`) and `.dlg-foot`. Cancel becomes `<a class="button ghost">`.
- Without JS, release dialogs keep their existing fallback (`?release=…#detail`).

## Bulk bar (#20, user fix 8)

The same `div.bulkbar[data-bulkbar][hidden]` inside the picker form. select.js shows it from the first selected row (no change).

```html
<div
  class="bulkbar"
  data-bulkbar
  hidden
  role="region"
  aria-label="Selected devices"
>
  <span class="bulk-count"><strong data-bar-count>0</strong> selected</span>
  <button type="button" class="ghost" data-pick="none" data-picker="picker">
    Clear selection
  </button>
  <div class="bulk-actions">
    Switch on · Switch off · Back up · Restart · Send command… · Make compliant
  </div>
  <details class="rowmenu up bulk-more">
    <summary class="button">Actions</summary>
    <div class="rowmenu-body">(same buttons)</div>
  </details>
</div>
```

- devices.html toolbar: remove "Select none", "Switch on selected", "Switch off selected" and `data-select-count`. Keep "Select all shown" and the colslot.
- firmware.html: the bar gets "Clear selection". Remove "Select none" and `data-select-count` from the toolbar.
- policy.html: "Make selected compliant" moves into a bar of its own.
- **server (now)**: `/devices/bulk/command` (Send command…). **later**: bulk `action=BACKUP|RESTART`; render those two buttons only once the server accepts them (`{{if .Features.BulkBackup}}`, `{{if .Features.BulkRestart}}`).
- The bar is `position: fixed`. While it is visible, the body gets `padding-bottom` equal to its height, so it never covers the table header or the last row.

## Columns chooser (#3): `tableprefs.go`

```diff
-menu = `<details class="colmenu" data-key="` + key + `"><summary class="colbtn" title="Show or hide columns" aria-label="Columns">` +
-	`</summary>` +
+menu = `<details class="colmenu" data-key="` + key + `"><summary class="colbtn" aria-label="Columns">Columns` +
+	`</summary>` +
```

The visible label replaces the icon and its tooltip. Labels in `.colpanel` wrap instead of clipping.

## Smaller markup changes

- update_batch.html `pre.log`: one `<span>` per line (`{{range .Log}}<span>{{.At}}  {{.Text}}</span>{{end}}`), for the hanging indent (#40).
- policy_rules.html: `.chips.scroll` stays. The CSS removes the fixed height (#65).
- firmware.html `devref` lists stay individual links. `.dev` no longer breaks inside a name, and on phones they are 24px targets in one wrapping run (#34 #84).
- Empty cells: tables.js already marks them with `td.none`. CSS shows "-" for `td:empty` (#51).

## Port notes added in the code repo (2026-10-21)

- **Theme (must)**: `styles.css` applies the dark tokens only under `:root[data-theme="dark"]`
  or, without JS, under `@media (prefers-color-scheme: dark) { :root[data-theme-auto] }`.
  `layout.html` must render `<html … data-theme-auto>` so "Auto" follows the system setting
  (light or dark, live when the system switches). An explicit Light/Dark choice sets
  `data-theme` and drops `data-theme-auto`. Add an e2e test: with `colorScheme: "dark"` and no
  explicit choice the page renders the dark tokens; with `"light"` the light ones.
- **CSRF (must)**: a GET no-JS fallback (`formmethod="get"` on a Delete… button) inside a POST
  form that carries the hidden `csrf` input would put the CSRF token into the URL (history,
  logs, Referer). Never do that: render the fallback as a link (`a.button.danger` to the
  confirmation page) or as a button in its own GET form without the token, and keep the
  dialog trigger separate. Add a test that no GET request carries `csrf=`.
