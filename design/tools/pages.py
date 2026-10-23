"""Generate the preview pages of ds-bundle (foundations, components, templates).

The markup is tasmofleet's (internal/web/templates); the data is a realistic
27-device fleet. Run after changing a page:

    python3 -I tools/pages.py
"""

import html
import os

ROOT = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "ds-bundle"
)
E = html.escape

# ---------------------------------------------------------------- icons (Lucide)
ICONS = {
    "check": '<path d="M20 6 9 17l-5-5"/>',
    "x": '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
    "chevron-down": '<path d="m6 9 6 6 6-6"/>',
    "info": '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
    "triangle-alert": '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    "power": '<path d="M12 2v10"/><path d="M18.4 6.6a9 9 0 1 1-12.77.04"/>',
    "wifi": '<path d="M12 20h.01"/><path d="M2 8.82a15 15 0 0 1 20 0"/><path d="M5 12.859a10 10 0 0 1 14 0"/><path d="M8.5 16.429a5 5 0 0 1 7 0"/>',
    "ellipsis": '<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>',
    "search": '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
    "external-link": '<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>',
    "plus": '<path d="M5 12h14"/><path d="M12 5v14"/>',
    "refresh-cw": '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
    "download": '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/>',
    "trash-2": '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>',
    "terminal": '<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>',
    "settings": '<path d="M20 7h-9"/><path d="M14 17H5"/><circle cx="17" cy="17" r="3"/><circle cx="7" cy="7" r="3"/>',
}


def icon(name, cls="ico"):
    return (
        f'<svg class="{cls}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" '
        f'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">{ICONS[name]}</svg>'
    )


EXT = icon("external-link")

# ---------------------------------------------------------------- page shells
DEMO_CSS = """    <style>
      /* Demo scaffolding only: everything visual comes from ../styles.css. */
      .demo { max-width: 620px; margin: 0 auto; padding: var(--space-6) var(--space-4) var(--space-8); }
      .demo-head { font-family: var(--font-body); font-size: 11px; letter-spacing: 0.1em; text-transform: uppercase;
        color: var(--color-text-2); margin: var(--space-6) 0 var(--space-3); }
      .demo-head:first-child { margin-top: 0; }
      .row { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
      .note { font-size: 12px; color: var(--color-text-2); max-width: 60ch; margin-top: var(--space-3); }
      .demo-foot { margin-top: var(--space-8); padding-top: var(--space-3); border-top: 1px solid var(--color-divider);
        display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); }
      .stage { position: relative; min-height: 220px; padding: var(--space-4); background: var(--color-bg);
        border: 1px solid var(--color-divider); overflow: hidden; }
    </style>"""

APP_CSS = """    <style>
      /* Demo scaffolding only: theme switch under the page. */
      .demo-foot { max-width: var(--container); margin: 0 auto; padding: 0 var(--page-pad) var(--space-8);
        display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); color: var(--color-text-2); font-size: 12px; }
    </style>"""

SWITCH = (
    '<div class="theme-switch" role="group" aria-label="Theme">'
    '<button type="button" class="sm" data-theme-set="system">Auto</button>'
    '<button type="button" class="sm" data-theme-set="light">Light</button>'
    '<button type="button" class="sm" data-theme-set="dark">Dark</button></div>'
)


def page(path, group, name, subtitle, body, height=560, app=False):
    marker = f'<!-- @dsCard group="{group}" name="{E(name)}" subtitle="{E(subtitle)}" viewport="640x{height}" -->'
    css = APP_CSS if app else DEMO_CSS
    if app:
        main = (
            body
            + f'\n<div class="demo-foot"><span>tasmofleet DS template, sample data</span>{SWITCH}</div>'
        )
    else:
        main = f'<main class="demo">\n{body}\n<div class="demo-foot"><span class="note">tasmofleet DS</span>{SWITCH}</div>\n</main>'
    doc = f"""{marker}
<!doctype html>
<html lang="en" data-theme-auto>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>tasmofleet DS — {E(name)}</title>
    <link rel="stylesheet" href="../styles.css" />
    <script src="../theme.js"></script>
{css}
  </head>
  <body>
{main}
  </body>
</html>
"""
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w") as f:
        f.write(doc)


# ---------------------------------------------------------------- data
STATES = {
    "online": ("ok", "Online"),
    "stale": ("warn", "Stale"),
    "offline": ("bad", "Offline"),
    "unknown": ("neutral", "Unknown"),
    "auth_failed": ("bad", "Auth failed"),
}
GLYPH = {"ok": "✓", "warn": "!", "bad": "✕", "info": "→", "neutral": "–"}

D = [
    # name, state, version, variant, family, ip, uptime, rssi, relays, seen, tags
    (
        "Balcony Windowsill",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.21",
        "12d 4h",
        78,
        ["ON"],
        "8s",
        ["outdoor"],
    ),
    (
        "Office Monitor Alice",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.22",
        "3d 7h",
        64,
        ["ON"],
        "4s",
        ["office"],
    ),
    (
        "Office Desk Lamp",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.23",
        "3d 7h",
        71,
        ["OFF"],
        "6s",
        ["office"],
    ),
    (
        "Kitchen Counter",
        "online",
        "15.1.0",
        "tasmota",
        "ESP8266",
        "172.31.13.24",
        "28d 2h",
        52,
        ["OFF"],
        "11s",
        ["kitchen"],
    ),
    (
        "Living Room Floor Lamp",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.25",
        "9d 1h",
        83,
        ["ON"],
        "3s",
        [],
    ),
    (
        "TV Backlight",
        "stale",
        "15.1.0",
        "tasmota",
        "ESP8266",
        "172.31.13.26",
        "1d 5h",
        38,
        ["OFF"],
        "14m",
        [],
    ),
    (
        "Bathroom Fan",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.27",
        "6h 12m",
        47,
        ["OFF"],
        "9s",
        ["bathroom"],
    ),
    (
        "Bedroom Nightstand",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.28",
        "12d 4h",
        69,
        ["OFF"],
        "5s",
        [],
    ),
    (
        "Hallway Ceiling",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.29",
        "21d 9h",
        90,
        ["ON"],
        "2s",
        [],
    ),
    (
        "Dehumidifier Basement",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.30",
        "4d 18h",
        41,
        ["ON"],
        "7s",
        ["basement"],
    ),
    (
        "Garden Pump",
        "stale",
        "15.0.1",
        "tasmota",
        "ESP8266",
        "172.31.13.31",
        "2d 3h",
        22,
        ["OFF"],
        "19m",
        ["outdoor"],
    ),
    (
        "Server Rack Fan",
        "online",
        "16.0.0",
        "tasmota32c3",
        "ESP32-C3",
        "172.31.13.32",
        "45d 6h",
        74,
        ["ON"],
        "1s",
        ["rack"],
    ),
    (
        "Washing Machine",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.33",
        "15d 3h",
        55,
        ["ON"],
        "4s",
        ["laundry"],
    ),
    (
        "Dryer",
        "online",
        "15.1.0",
        "tasmota",
        "ESP8266",
        "172.31.13.34",
        "15d 3h",
        49,
        ["OFF"],
        "8s",
        ["laundry"],
    ),
    (
        "Coffee Machine",
        "online",
        "16.0.0",
        "tasmota",
        "ESP8266",
        "172.31.13.35",
        "2d 22h",
        66,
        ["OFF"],
        "3s",
        ["kitchen"],
    ),
    (
        "Aquarium Light",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.36",
        "30d 1h",
        80,
        ["ON"],
        "6s",
        [],
    ),
    (
        "Christmas Tree",
        "unknown",
        None,
        None,
        None,
        "172.31.13.37",
        None,
        None,
        [],
        None,
        ["seasonal"],
    ),
    (
        "Stairway Dual Switch",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.13.38",
        "10d 0h",
        59,
        ["ON", "OFF"],
        "5s",
        [],
    ),
    (
        "Workshop Compressor",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.23.40",
        "7d 2h",
        58,
        ["OFF"],
        "12s",
        ["workshop"],
    ),
    (
        "Workshop Ceiling Light",
        "online",
        "16.0.0",
        "tasmota32",
        "ESP32",
        "172.31.23.41",
        "7d 2h",
        61,
        ["ON"],
        "10s",
        ["workshop"],
    ),
] + [
    (
        f"Fairylights {n}",
        "offline",
        "15.1.0",
        "tasmota",
        "ESP8266",
        f"172.31.13.{49 + n}",
        None,
        None,
        ["?"],
        "3d",
        ["seasonal"],
    )
    for n in range(1, 8)
]
assert len(D) == 27
COUNT = {s: sum(1 for d in D if d[1] == s) for s in STATES}


def bars(rssi):
    return 4 if rssi >= 75 else 3 if rssi >= 55 else 2 if rssi >= 35 else 1


def badge(state, extra=""):
    tone, lab = STATES[state]
    st = " st-unknown" if state == "unknown" else ""
    return f'<span class="badge tone-{tone}{st}"{extra}>{lab}</span>'


def relay_btns(relays, enabled=True):
    if not relays:
        return '<span class="muted">-</span>'
    out = []
    for i, st in enumerate(relays, 1):
        cls = " on" if st == "ON" else "" if st == "OFF" else " unk"
        lab = {"ON": "On", "OFF": "Off"}.get(st, "Unknown")
        pre = f"{i}: " if len(relays) > 1 else ""
        dis = " disabled" if st == "?" else ""
        out.append(
            f'<span class="relay"><button type="button" class="relay-btn{cls}" aria-pressed="{str(st == "ON").lower()}"'
            f' aria-label="Toggle relay {i}, currently {lab}"{dis}>{pre}{lab}</button></span>'
        )
    return " ".join(out)


def addr(ip):
    return f'<a class="ext" href="http://{ip}/" target="_blank" rel="noopener noreferrer">{ip}{EXT}<span class="sr-only"> (opens in a new tab)</span></a>'


TOP = """<header class="top">
<a class="brand" href="dashboard.html"><img src="../assets/logo.svg" alt="" width="26" height="26">tasmofleet</a>
<nav aria-label="Main" class="mainnav">
<a class="navtop{d}" href="dashboard.html"{dc}>Dashboard</a>
<details class="navgroup"><summary class="navtop{v}">Devices</summary>
<div class="menu"><a href="devices.html"{vc}>Devices</a><a href="#">Add devices</a><a href="#">Policy</a></div></details>
<details class="navgroup"><summary class="navtop{u}">Updates</summary>
<div class="menu"><a href="#">New transaction</a><a href="#">Transactions</a><a href="firmware.html"{uc}>Firmware</a></div></details>
<details class="navgroup"><summary class="navtop">System</summary>
<div class="menu right"><a href="#">Settings</a><a href="#">Users</a><a href="#">Audit</a><a href="#">About</a></div></details>
</nav>
<div class="user">
<details class="navgroup"><summary class="who">alice <small>(admin)</small></summary>
<div class="menu right"><a href="#">Change password</a></div></details>
<form method="post" action="#"><button type="submit" class="sm">Sign out</button></form>
</div>
</header>"""


def top(nav):
    cur = ' aria-current="page"'
    return TOP.format(
        d=" is-active" if nav == "dashboard" else "",
        dc=cur if nav == "dashboard" else "",
        v=" is-active" if nav == "devices" else "",
        vc=cur if nav == "devices" else "",
        u=" is-active" if nav == "firmware" else "",
        uc=cur if nav == "firmware" else "",
    )


def device_row(i, d, selected=False, menu_open=False):
    name, state, ver, var, fam, ip, up, rssi, relays, seen, tags = d
    chk = " checked" if selected else ""
    fw = (
        f'{ver} <small class="muted">{var}</small>'
        if ver
        else '<span class="muted">Unknown</span>'
    )
    famc = fam or '<span class="muted">Unknown</span>'
    upc = f"<span>{up}</span>" if up else '<span class="muted">-</span>'
    wifi = (
        (
            f'<span class="wifi"><span class="sig sig{bars(rssi)}" aria-hidden="true"><i></i><i></i><i></i><i></i></span>'
            f'<span class="pct">{rssi}%</span></span>'
        )
        if rssi
        else '<span class="muted">-</span>'
    )
    seen_c = (
        f' <small class="muted">{seen}</small>' if seen and state != "online" else ""
    )
    tag = ' <span class="tag">no OTA</span>' if name == "Server Rack Fan" else ""
    op = " open" if menu_open else ""
    return f"""<tr>
<td data-label="Select" class="sel"><label class="hit"><input type="checkbox" name="id" value="{i}" aria-label="Select {E(name)}"{chk}></label></td>
<td data-label="Name" class="wrap p1"><a href="device.html">{E(name)}</a>{tag}</td>
<td data-label="Status" class="nowrap s">{badge(state)}{seen_c}</td>
<td data-label="Firmware" class="nowrap s">{fw}</td>
<td data-label="Family" class="nowrap c-fam m-hide">{famc}</td>
<td data-label="Address" class="nowrap">{addr(ip)}</td>
<td data-label="Uptime" class="nowrap s c-up">{upc}</td>
<td data-label="Wi-Fi" class="nowrap s c-wifi">{wifi}</td>
<td data-label="Power" class="nowrap p2">{relay_btns(relays)}</td>
<td data-label="Actions" class="end rowact"><details class="rowmenu"{op}><summary class="button sm">Actions</summary><div class="rowmenu-body">
<a href="#">{icon("terminal")}Console</a><a href="#">{icon("settings")}Settings</a><button type="button">{icon("download")}Back up now</button><button type="button">{icon("refresh-cw")}Restart</button><hr><a href="#" class="danger">{icon("trash-2")}Delete…</a></div></details></td>
</tr>"""


def device_table(rows, head_sel=True):
    return f"""<div class="table-scroll"><table class="devtable dense" data-table="devices">
<thead><tr><th class="sel"><label class="hit"><input type="checkbox" aria-label="Select all shown devices"></label></th>
<th aria-sort="ascending"><a class="sortlink is-asc" href="#">Name<span class="sortmark" aria-hidden="true"></span></a></th>
<th><a class="sortlink" href="#">Status<span class="sortmark" aria-hidden="true"></span></a></th>
<th><a class="sortlink" href="#">Firmware<span class="sortmark" aria-hidden="true"></span></a></th>
<th class="c-fam">Family</th><th>Address</th>
<th class="c-up"><a class="sortlink" href="#">Uptime<span class="sortmark" aria-hidden="true"></span></a></th>
<th class="c-wifi"><a class="sortlink" href="#">Wi-Fi<span class="sortmark" aria-hidden="true"></span></a></th>
<th>Power</th><th class="end"><span class="sr-only">Actions</span></th></tr></thead>
<tbody>
{rows}
</tbody></table></div>"""


# ================================================================ foundations
def foundations():
    # ---- type
    page(
        "foundations/type.html",
        "Foundations",
        "Type",
        "Archivo only; dense admin scale, body 14px",
        """<p class="demo-head">Scale</p>
<h1>Devices — h1 26px / 800</h1>
<h2>Fleet health — h2 19px / 700</h2>
<h3>Files this fleet needs — h3 16px / 700</h3>
<p>Body 14px / 1.5, weight 400. Balcony Windowsill reports to the broker and answered the last HTTP poll; firmware 16.0.0 is the recommended release.</p>
<p><small>Small 12px: hints, badges, table headers. </small><span class="muted">Muted text uses --color-text-2.</span></p>
<p class="demo-head">Table and data</p>
<table class="dense"><thead><tr><th>Header 11px caps</th><th>Cell 13px</th><th class="end">Numbers</th></tr></thead>
<tbody><tr><td class="p1"><a href="#">Office Monitor Alice</a></td><td>16.0.0 <small class="muted">tasmota32</small></td><td class="end">1 024 KiB</td></tr></tbody></table>
<p class="demo-head">Mono</p>
<p><code>sha256 8f704806dbed</code> uses the system mono stack (hashes, commands).</p>
<p class="note">Tabular figures are on everywhere (font-variant-numeric: tabular-nums), so uptimes and percentages line up in columns.</p>""",
        640,
    )

    # ---- color
    def ramp(name):
        cells = "".join(
            f'<div class="sw{" d" if s >= 500 else ""}" style="--c: var(--color-{name}-{s})"><span>{s}</span></div>'
            for s in (100, 200, 300, 400, 500, 600, 700, 800, 900)
        )
        return f'<div class="ramp"><span class="rl">{name}</span>{cells}</div>'

    roles = "".join(
        f'<div class="role"><span class="badge tone-{t}{" st-unknown" if t == "neutral" else ""}">{lab}</span><span class="note">{what}</span></div>'
        for t, lab, what in [
            ("ok", "Online", "success: online, compliant, succeeded"),
            ("warn", "Stale", "warning: stale, deviating, downgrade"),
            ("bad", "Offline", "danger: offline, failed, auth failed — the only red"),
            ("info", "Updating", "info: in progress, new"),
            ("neutral", "Unknown", "neutral: unknown, queued, cancelled"),
        ]
    )
    page(
        "foundations/color.html",
        "Foundations",
        "Color",
        "Ink on ground; teal accent; five status roles with ramps",
        f"""<style>
  .ramp {{ display: grid; grid-template-columns: 64px repeat(9, 1fr); gap: 2px; align-items: stretch; margin-bottom: 2px; }}
  .rl {{ font-size: 11px; font-weight: 700; align-self: center; }}
  .sw {{ background: var(--c); height: 34px; display: flex; align-items: flex-end; padding: 2px 4px; }}
  .sw span {{ font-size: 10px; color: #000; }} .sw.d span {{ color: #fff; }}
  .surf {{ display: grid; grid-template-columns: repeat(4, 1fr); gap: 0; border: 1px solid var(--color-divider); }}
  .surf div {{ padding: 10px; min-height: 64px; font-size: 12px; border-right: 1px solid var(--color-divider); }}
  .role {{ display: flex; gap: 12px; align-items: center; margin-bottom: 6px; }}
  .role .note {{ margin: 0; }}
</style>
<p class="demo-head">Surfaces (this theme)</p>
<div class="surf"><div style="background:var(--color-bg)">bg<br><small class="muted">page ground</small></div>
<div style="background:var(--color-surface)">surface<br><small class="muted">cards, tables</small></div>
<div style="background:var(--color-surface-2)">surface-2<br><small class="muted">sunk, empty state</small></div>
<div style="background:var(--color-overlay);box-shadow:var(--shadow-lg);border:1px solid var(--color-border-overlay)">overlay<br><small class="muted">menus, dialogs</small></div></div>
<p class="demo-head">Ink and accent</p>
<div class="row"><button class="primary">Accent action</button><a href="#">A link</a><span class="badge accent">recommended</span>
<span style="border-bottom:2px solid var(--color-rule);padding-bottom:2px;font-weight:700">2px rule = ink</span></div>
<p class="demo-head">Status roles</p>
{roles}
<p class="demo-head">Ramps (OKLCH, shared lightness scale)</p>
{"".join(ramp(n) for n in ("accent", "success", "warning", "danger", "info", "neutral"))}
<p class="note">Light uses accent-700 (#0f766e), dark accent-400 (#2dd4bf). Colour is never the only signal: every status has a glyph and a word.</p>""",
        640,
    )

    # ---- layout
    page(
        "foundations/layout.html",
        "Foundations",
        "Layout",
        "12-column modular grid, 4px spacing, 2px section rules, 0 radius",
        """<style>
  .cols12 { display: grid; grid-template-columns: repeat(12, 1fr); gap: var(--gutter); height: 80px; }
  .cols12 i { background: color-mix(in srgb, var(--color-accent) 12%, transparent); border-left: 1px solid var(--color-accent); }
  .sp { display: flex; align-items: flex-end; gap: 10px; }
  .sp > div > div { background: var(--color-text); }
  .sp span { display: block; font-size: 10px; color: var(--color-text-2); margin-top: 4px; }
</style>
<p class="demo-head">Grid: 12 columns, 16px gutter, max 1440px</p>
<div class="cols12"><i></i><i></i><i></i><i></i><i></i><i></i><i></i><i></i><i></i><i></i><i></i><i></i></div>
<p class="demo-head">Spacing (4px base)</p>
<div class="sp">
<div><div style="width:4px;height:4px"></div><span>1</span></div><div><div style="width:8px;height:8px"></div><span>2</span></div>
<div><div style="width:12px;height:12px"></div><span>3</span></div><div><div style="width:16px;height:16px"></div><span>4</span></div>
<div><div style="width:20px;height:20px"></div><span>5</span></div><div><div style="width:24px;height:24px"></div><span>6</span></div>
<div><div style="width:32px;height:32px"></div><span>8</span></div><div><div style="width:40px;height:40px"></div><span>10</span></div>
<div><div style="width:48px;height:48px"></div><span>12</span></div></div>
<p class="demo-head">Rules and frames</p>
<section class="card"><h2>Card: 2px ink rule on top, title in the frame</h2><p class="muted">Hairline divider on the other sides. Content starts 24px below the rule, so the title never touches it.</p></section>
<p class="demo-head">Controls: one height per size</p>
<div class="row"><button class="sm">28 sm</button><button>34 md</button><button class="lg">40 lg</button><input style="width:9rem" value="34 input"></div>
<p class="note">Breakpoints: phone up to 640px (tables become row blocks, nav wraps under the brand), two columns from 1100px.</p>""",
        640,
    )

    # ---- icons
    names = list(ICONS)
    grid = "".join(f'<div class="ic">{icon(n)}<span>{n}</span></div>' for n in names)
    page(
        "foundations/icons.html",
        "Foundations",
        "Icons",
        "Lucide, 16px, stroke 2; status glyphs in text",
        f"""<style>
  .icg {{ display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 0; border-top: 2px solid var(--color-rule); }}
  .ic {{ display: flex; gap: 8px; align-items: center; padding: 10px 6px; border-bottom: 1px solid var(--color-divider); font-size: 12px; }}
  .glyphs {{ display: grid; grid-template-columns: repeat(auto-fill, minmax(104px, 1fr)); gap: 8px; }}
  .glyphs div {{ display: grid; gap: 6px; justify-items: start; }}
</style>
<p class="demo-head">Status glyphs (CSS ::before, never colour alone)</p>
<div class="glyphs">
<div>{badge("online")}<small class="muted">✓ success</small></div>
<div>{badge("stale")}<small class="muted">! warning</small></div>
<div>{badge("offline")}<small class="muted">✕ danger</small></div>
<div><span class="badge tone-info">Updating</span><small class="muted">→ info</small></div>
<div>{badge("unknown")}<small class="muted">? / – neutral</small></div>
</div>
<p class="demo-head">Interface icons (inline Lucide SVG)</p>
<div class="icg">{grid}</div>
<p class="note">Inline SVG with stroke="currentColor", class "ico"; decorative icons get aria-hidden. Icon-only buttons (button.icon) need an aria-label.</p>""",
        600,
    )


# ================================================================ components
def components():
    page(
        "components/buttons.html",
        "Components",
        "Buttons",
        "Centered labels, one height per size; block buttons flush left",
        f"""<p class="demo-head">Variants</p>
<div class="row"><button class="primary">Add devices</button><button>Refresh index</button><button class="danger">Delete device</button><button class="ghost">Cancel</button>
<button class="icon" aria-label="Refresh">{icon("refresh-cw")}</button></div>
<p class="demo-head">Sizes: 28 / 34 / 40</p>
<div class="row"><button class="sm">Select all</button><button>Filter</button><button class="lg primary">Sign in</button></div>
<p class="demo-head">States (default, hover, active, focus-visible, disabled, busy, pressed)</p>
<div class="row"><button class="primary">Default</button><button class="primary is-hover">Hover</button><button class="primary is-active">Active</button>
<button class="primary is-focus">Focus</button><button class="primary" disabled>Disabled</button><button class="primary" aria-busy="true">Saving</button></div>
<div class="row" style="margin-top:8px"><button>Default</button><button class="is-hover">Hover</button><button class="is-active">Active</button><button class="is-focus">Focus</button>
<button disabled>Disabled</button><button aria-pressed="true">Pressed</button></div>
<div class="row" style="margin-top:8px"><button class="danger">Default</button><button class="danger is-hover">Hover</button><button class="danger" disabled>Disabled</button>
<a class="button sm" href="#">Link as button</a><span class="button sm is-off" aria-disabled="true">Next</span><span class="button sm is-current" aria-current="page">3</span></div>
<p class="demo-head">In a table row: compact, centered</p>
<div class="row"><button class="sm">Restore</button><button class="sm danger">Delete…</button><button class="sm" style="width:12rem">Wider, still centered</button></div>
<p class="demo-head">Block (full width): flush left</p>
<button class="block primary">{icon("plus")}Add 4 discovered devices</button>
<p class="note">One primary per view. Labels are centered in compact and in-table buttons; only .block buttons keep Modernist's flush-left label. Height is fixed per size (height, not min-height).</p>""",
        640,
    )

    page(
        "components/forms.html",
        "Components",
        "Forms",
        "Label, help hint, (i) info button, errors; all states",
        f"""<form class="stack" action="#">
<label><span class="lbl">Device name {INFO("Shown in lists and the audit log. Up to 64 characters.")}</span><input value="Balcony Windowsill"><small>Shown in lists and the audit log.</small></label>
<label>Address <input value="172.31.13.21" class="is-focus"><small>IP or host name; the port defaults to 80.</small></label>
<label>Firmware version <input value="16.0" aria-invalid="true" aria-describedby="v-err"><small class="error" id="v-err">Use dotted numbers, for example 16.0.0.</small></label>
<label>Power after update <select><option>Restore lights</option><option>Restore lights and relays</option><option>Leave as is</option></select></label>
<label>Notes <textarea placeholder="Optional">Mounted behind the planter.</textarea></label>
<label>Disabled <input value="MQTT topic set by policy" disabled></label>
<label class="inline"><input type="checkbox" checked> Exclude from updates</label>
<label class="inline"><input type="checkbox"> Single-core ESP32 build</label>
<fieldset><legend>Credentials</legend>
<label>User <input value="admin"></label><label>Password <input type="password" value="secret12"></label></fieldset>
<p class="actions"><button type="submit" class="primary">Save</button><button type="button" class="ghost">Cancel</button></p>
</form>
<p class="demo-head">Filters row</p>
<form class="filters" action="#"><label class="grow">Search <input type="search" placeholder="Name, address or tag"></label>
<label>Health <select><option>Any</option><option>Online</option></select></label><button type="submit">Filter</button></form>
<p class="note">Labels ≤ 6 words; explanations go into the (i) tooltip. Placeholder text uses text-2 (AA). Invalid fields get a danger border, a 3px inner rule and a text message.</p>""",
        640,
    )

    page(
        "components/cards.html",
        "Components",
        "Cards & stat tiles",
        "Framed legend title on a 2px rule; stat tiles; facts; empty state",
        f"""<section class="card"><h2>Identity</h2>
<dl class="facts"><dt>Address</dt><dd>{addr("172.31.13.21")}</dd><dt>MAC</dt><dd>A4:CF:12:3B:9E:01</dd><dt>Chip family</dt><dd>ESP8266</dd>
<dt>Firmware</dt><dd>16.0.0 <small>tasmota en</small></dd><dt>Tags</dt><dd><span class="tag">outdoor</span></dd></dl></section>
<section class="card"><h2>Fleet health</h2>
<ul class="stats">
<li><a class="stat tone-ok" href="#"><span class="num">{COUNT["online"]}</span><span class="what" data-glyph="✓">Online</span></a></li>
<li><a class="stat tone-warn" href="#"><span class="num">{COUNT["stale"]}</span><span class="what" data-glyph="!">Stale</span></a></li>
<li><a class="stat tone-bad" href="#"><span class="num">{COUNT["offline"]}</span><span class="what" data-glyph="✕">Offline</span></a></li>
<li><a class="stat tone-neutral zero" href="#"><span class="num">0</span><span class="what" data-glyph="✕">Auth failed</span></a></li>
</ul><p class="muted summary">27 devices in total.</p></section>
<section class="card"><h2>Backups</h2>
<div class="empty-state"><strong>No backups yet.</strong>Take one before you change or update this device.<button class="sm primary">Back up now</button></div></section>
<section class="card danger-zone"><h2>Delete device</h2><p class="muted">Removes Balcony Windowsill and its history from tasmofleet. The device itself is not touched.</p><button class="danger">Delete device</button></section>
<p class="note">The title is the card's first child (h2 or .card-head) and sits in the frame. Padding-top 24px keeps content clear of it. Stat tiles: 4px tone bar, glyph + word, zero values greyed.</p>""",
        640,
    )

    page(
        "components/navigation.html",
        "Components",
        "Navigation",
        "Top bar with grouped menus, page header with one primary action, mobile top nav",
        f"""<style>.frame {{ border: 1px solid var(--color-divider); margin-bottom: 8px; }} .frame .top {{ padding: 0 12px; }} .tall {{ min-height: 190px; }}</style>
<p class="demo-head">Top bar (desktop), Devices menu open</p>
<div class="frame tall">
<header class="top"><a class="brand" href="#"><img src="../assets/logo.svg" alt="" width="26" height="26">tasmofleet</a>
<nav aria-label="Main" class="mainnav"><a class="navtop" href="#">Dashboard</a>
<details class="navgroup" open><summary class="navtop is-active">Devices</summary><div class="menu"><a href="#" aria-current="page">Devices</a><a href="#" class="is-hover">Add devices</a><a href="#">Policy</a></div></details>
<details class="navgroup"><summary class="navtop">Updates</summary><div class="menu"><a href="#">Firmware</a></div></details></nav>
<div class="user"><details class="navgroup"><summary class="who">alice <small>(admin)</small></summary><div class="menu right"><a href="#">Change password</a></div></details></div></header></div>
<p class="demo-head">Page header: title (+ status), crumb, one primary action right</p>
<div class="page-head"><h1>Balcony Windowsill {badge("online")}</h1>
<div class="actions"><a class="button" href="#">Console</a><a class="button primary" href="#">Settings</a></div>
<p class="crumb"><a href="#">Devices</a></p></div>
<div class="page-head"><h1>Devices</h1><div class="actions"><a class="button primary" href="#">{icon("plus")}Add devices</a></div></div>
<p class="demo-head">Pager</p>
<nav class="pager" aria-label="Pages"><a class="button sm" href="#">Previous</a><a class="button sm" href="#">1</a><span class="button sm is-current" aria-current="page">2</span><a class="button sm" href="#">3</a><a class="button sm" href="#">Next</a><span class="muted">Page 2 of 3, 27 devices</span></nav>
<p class="note">At ≤ 640px the navigation moves to a second row under brand and user (see the templates at 375px); group menus open full width.</p>""",
        640,
    )

    rows = "\n".join(
        device_row(i, D[i], selected=(i in (2,)), menu_open=False) for i in range(4)
    )
    rows = rows.replace('class="nowrap c-fam m-hide"', 'class="nowrap c-fam" hidden').replace(
        '<td data-label="Address" class="nowrap">', '<td data-label="Address" class="nowrap" hidden>'
    )
    page(
        "components/table.html",
        "Components",
        "Data table",
        "Dense list: selection, sortable headers, row actions (≤ 2 buttons, else menu)",
        f"""<div class="toolbar" role="group" aria-label="Selection"><button type="button" class="sm">Select all shown</button><button type="button" class="sm">Select none</button>
<span class="muted" aria-live="polite">1 of 27 selected</span></div>
{device_table(rows).replace('<th class="c-fam">Family</th><th>Address</th>', '')}
<p class="demo-head">Row actions: at most two buttons; destructive with others → menu</p>
<table class="dense"><thead><tr><th>Taken</th><th>Kind</th><th>Size</th><th class="end"><span class="sr-only">Actions</span></th></tr></thead><tbody>
<tr><td class="p1" data-label="Taken">2026-10-20 06:00</td><td class="s" data-label="Kind">Config file</td><td class="s" data-label="Size">4.1 KiB</td>
<td class="p2 end" data-label="Actions"><div class="row-actions"><a class="button sm" href="#">Restore</a>
<details class="rowmenu"><summary class="button sm icon" aria-label="More actions">{icon("ellipsis")}</summary><div class="rowmenu-body"><a href="#">Download</a><button type="button" class="danger">Delete…</button></div></details></div></td></tr>
<tr class="is-hover"><td class="p1" data-label="Taken">2026-10-13 06:00</td><td class="s" data-label="Kind">Snapshot</td><td class="s" data-label="Size">12.6 KiB</td>
<td class="p2 end" data-label="Actions"><div class="row-actions"><a class="button sm" href="#">Download</a></div></td></tr>
<tr><td colspan="4" class="empty"><strong>No devices match.</strong><br>Clear the filters or add a device.</td></tr>
</tbody></table>
<p class="note">Selected rows: soft accent fill + 3px accent bar (not colour alone: the checkbox is checked). Sort state: accent arrow + aria-sort. Under 640px each row becomes a block: name and power on top, the other cells as label/value lines.</p>""",
        640,
    )

    page(
        "components/dialog.html",
        "Components",
        "Dialog",
        "Overlay surface with border and shadow over a dimmed backdrop",
        """<div class="stage" style="min-height:360px">
<p>Firmware page content behind the dialog…</p>
<div class="backdrop"></div>
<dialog class="dlg" open aria-labelledby="dlg-h" style="position:absolute;inset:24px auto auto 50%;transform:translateX(-50%);margin:0;width:min(520px,90%)">
<div class="dlg-head"><h2 id="dlg-h">Release 16.0.0 <span class="badge accent">recommended</span></h2><button type="button" data-close>Close</button></div>
<div class="dlg-body"><h3>Fleet</h3><ul><li>Older than this release (6): Kitchen Counter, TV Backlight, Garden Pump, Dryer, …</li><li>Same version (13)</li></ul>
<p class="muted">The release notes were cut at 32 KiB.</p></div>
<div class="dlg-foot"><button type="button" class="ghost">Cancel</button><button type="button" class="primary">Prefetch for my fleet</button></div>
</dialog></div>
<p class="note">dialog.dlg with .dlg-head, .dlg-body and .dlg-foot (NEW). The backdrop is ::backdrop in the app; .backdrop only reproduces it in previews. On phones the dialog becomes a bottom sheet.</p>""",
        520,
    )

    page(
        "components/menus.html",
        "Components",
        "Menus",
        "Dropdown and Actions menu on the overlay surface; item states",
        f"""<div class="stage" style="min-height:300px">
<div class="row" style="justify-content:space-between;align-items:flex-start">
<details class="rowmenu" open><summary class="button sm">Actions</summary><div class="rowmenu-body" style="left:0;right:auto">
<a href="#">{icon("terminal")}Console</a><a href="#" class="is-hover">{icon("settings")}Settings (hover)</a><button type="button" class="is-focus">{icon("download")}Back up now (focus)</button>
<button type="button" disabled>{icon("refresh-cw")}Restart (disabled)</button><hr><a href="#" class="danger">{icon("trash-2")}Delete…</a></div></details>
<details class="navgroup" open><summary class="button sm">Updates</summary><div class="menu right"><a href="#">New transaction</a><a href="#">Transactions</a><a href="#" aria-current="page">Firmware</a><span class="menu-note">Firmware is cached for 30 days.</span></div></details>
</div></div>
<p class="note">Menu items are list rows: flush left, full width, 32px. Current item: bold + 3px accent bar. Destructive items in danger text, separated by a rule. Open trigger is inverted (ink).</p>""",
        420,
    )

    page(
        "components/tooltip.html",
        "Components",
        "Tooltip & popover",
        "Overlay surface, 1px border, shadow; ≤ 160 characters",
        f"""<div class="stage" style="min-height:240px">
<p><label style="display:inline-flex"><span class="lbl">Power after update {INFO("Which relays tasmofleet switches back on after the device rebooted.")}</span></label></p>
<div class="tooltip is-static" role="tooltip">Which relays tasmofleet switches back on after the device rebooted.</div>
<p style="margin-top:16px">{badge("stale")} <small class="muted">14m</small></p>
<div class="tooltip is-static" role="tooltip">Stale: no report for more than 11 minutes.</div>
<div class="popover" style="margin-top:16px;max-width:18rem"><strong>Columns</strong>
<label class="inline"><input type="checkbox" checked> Uptime</label><label class="inline"><input type="checkbox" checked> Wi-Fi</label><label class="inline"><input type="checkbox"> Last restart</label></div>
</div>
<p class="note">Tooltips explain, they never list data: one or two sentences. They use the overlay surface (no longer inverted), so they read the same as menus in both themes.</p>""",
        480,
    )

    page(
        "components/tabs.html",
        "Components",
        "Tabs",
        "Underline tabs; a select replaces them on phones",
        """<div class="jump"><select aria-label="Add devices"><option selected>Scan and discovered</option><option>Add manually</option><option>Import from TasmoAdmin</option></select></div>
<nav class="tabs" aria-label="Add devices"><a href="#" aria-current="page">Scan and discovered<span class="count">4</span></a><a href="#" class="is-hover">Add manually</a><a href="#" class="is-focus">Import from TasmoAdmin</a><a href="#" aria-disabled="true">Zeroconf</a></nav>
<p>Discovered devices appear here.</p>
<p class="note">States shown: selected (ink + 3px accent), hover, focus-visible, disabled. Below 640px the .jump select (placed right before the tabs) replaces them; in the app jump.js unhides it, so without script the tabs stay.</p>""",
        420,
    )

    page(
        "components/badges.html",
        "Components",
        "Status badges & tags",
        "Glyph + word + colour; five roles, CVD safe",
        f"""<p class="demo-head">Device health</p>
<div class="row">{badge("online")}{badge("stale")}{badge("offline")}{badge("auth_failed")}{badge("unknown")}</div>
<p class="demo-head">Transactions and policy</p>
<div class="row"><span class="badge tone-info">Flashing</span><span class="badge tone-ok">Succeeded</span><span class="badge tone-bad">Failed</span><span class="badge tone-neutral">Queued</span>
<span class="badge tone-ok">Compliant</span><span class="badge tone-warn">Deviating</span></div>
<p class="demo-head">Accent badge and tags</p>
<div class="row"><span class="badge accent">recommended</span><span class="tag">outdoor</span><span class="tag">no OTA</span><span class="tag">seasonal</span></div>
<p class="demo-head">In a heading</p>
<div class="page-head"><h1>Fairylights 3 {badge("offline")}</h1></div>
<p class="note">Glyphs: ✓ success, ! warning, ✕ danger, → info, ? unknown, – neutral. Red appears only for danger. The accent badge is the one non-status badge.</p>""",
        460,
    )

    page(
        "components/toggles.html",
        "Components",
        "Power toggle",
        "On / off / unknown; ink, not a status colour",
        """<p class="demo-head">States</p>
<div class="row"><span class="relay"><button class="relay-btn on" aria-pressed="true" aria-label="Toggle relay 1, currently On">On</button></span>
<span class="relay"><button class="relay-btn" aria-pressed="false" aria-label="Toggle relay 1, currently Off">Off</button></span>
<span class="relay"><button class="relay-btn unk" aria-pressed="false" aria-label="Toggle relay 1, currently unknown">Unknown</button></span></div>
<p class="demo-head">Hover, focus, busy, disabled</p>
<div class="row"><button class="relay-btn on is-hover">On</button><button class="relay-btn is-hover">Off</button><button class="relay-btn is-focus">Off</button>
<button class="relay-btn on" aria-busy="true">On</button><button class="relay-btn unk" disabled>Unknown</button></div>
<p class="demo-head">Two relays; read-only</p>
<div class="row"><span class="relay"><button class="relay-btn on">1: On</button></span> <span class="relay"><button class="relay-btn">2: Off</button></span> <span class="relay-state">On</span></div>
<p class="note">Filled square = on, empty square = off, "?" with a dashed border = unknown (offline device, disabled). The word is always there. Ink instead of amber: amber is the warning role.</p>""",
        420,
    )

    page(
        "components/alerts.html",
        "Components",
        "Inline alerts",
        "Info, success, warning, error: glyph, tone rule, soft fill",
        """<p class="notice plain" role="status">This device is being updated: Flashing. It does not answer while it downloads and reboots, so this is not a failure. <a href="#">Follow the update</a></p>
<p class="notice" role="status">Identity refreshed.</p>
<p class="warning" role="status">The setup is not finished. <a href="#">Open the setup</a></p>
<p class="error" role="alert">Fairylights 3 did not answer at 172.31.13.52:80 (timeout after 5 s).</p>
<p class="demo-head">Inline text</p>
<p><span class="error">not offered upstream</span> · <small class="error">Use dotted numbers.</small></p>
<p class="note">.notice.plain = info, .notice = success, .warning, .error. Use role="status" for info/success/warning and role="alert" for errors.</p>""",
        460,
    )

    rows = "\n".join(device_row(i, D[20 + i], selected=True) for i in range(3))
    page(
        "components/bulkbar.html",
        "Components",
        "Bulk-action bar",
        "Sticky bar with count and actions while rows are selected",
        f"""{device_table(rows)}
<div class="bulkbar" data-bulkbar role="region" aria-label="Selected devices">
<span class="bulk-count"><strong data-bar-count>3</strong> selected</span>
<button type="button" class="ghost">Clear selection</button><button type="submit">Switch off</button><button type="submit" class="primary">Switch on</button></div>
<p class="note">Overlay surface, 2px accent rule on top. Sticks to the bottom of the window; actions in order Clear selection, secondary, primary last; on phones the actions share the width.</p>""",
        460,
    )

    page(
        "components/collapsible.html",
        "Components",
        "Collapsible section",
        "Collapsed shows only the header; fold inside a card",
        f"""<details class="fold" open><summary>State changes<span class="meta">12 changes</span></summary>
<table class="dense"><tbody><tr><td class="s" data-label="When">2026-10-20 06:14</td><td class="p1" data-label="Change">{badge("offline")} → {badge("online")}</td></tr></tbody></table></details>
<details class="fold"><summary>Transactions<span class="meta">3 transactions</span></summary><p>Hidden content</p></details>
<details class="fold"><summary>Backups<span class="meta">8 backups</span></summary><p>Hidden content</p></details>
<section class="card"><details class="m-fold fold-h"><summary><h2>Custom images</h2></summary><p>Hidden content</p></details></section>
<section class="card"><details class="m-fold fold-h" open><summary><h2>Prefetch a file</h2></summary>
<form class="filters" action="#"><label>Release <input placeholder="16.0.0"></label><label>File <input placeholder="tasmota.bin.gz"></label><button class="primary">Fetch</button></form></details></section>
<p class="note">details.fold: 2px rule on top, chevron, optional .meta on the right. In a card (.fold-h) the summary is the framed title; collapsed, the card shrinks to its frame.</p>""",
        560,
    )


def INFO(text):
    return f'<button type="button" class="info" aria-label="More information" title="{E(text)}"><span aria-hidden="true">i</span></button>'


# ================================================================ templates
def templates():
    # ---- devices
    rows = "\n".join(
        device_row(
            i,
            d,
            selected=d[0] in ("Fairylights 1", "Fairylights 2", "Fairylights 3"),
            menu_open=(i == 1),
        )
        for i, d in enumerate(sorted(D, key=lambda d: d[0]))
    )
    body = f"""{top("devices")}
<main>
<div class="page-head"><h1>Devices</h1><div class="actions"><a class="button primary" href="#">{icon("plus")}Add devices</a></div></div>
<details class="m-fold" open><summary>Filters</summary>
<form class="filters" method="get" action="#" aria-label="Filter devices">
<label class="grow">Search <input type="search" name="q" placeholder="Name, address or tag"></label>
<label>Health <select name="health"><option>Any</option><option>Online</option><option>Stale</option><option>Offline</option></select></label>
<label>Family <select name="family"><option>Any</option><option>ESP32</option><option>ESP8266</option></select></label>
<label>Tag <select name="tag"><option>Any</option><option>seasonal</option></select></label>
<button type="submit">Filter</button></form></details>
<form id="picker" method="post" action="#">
<div class="toolbar" role="group" aria-label="Selection"><button type="button" class="sm">Select all shown</button><button type="button" class="sm">Select none</button>
<span class="muted" aria-live="polite">3 of 27 selected</span></div>
{device_table(rows)}
<div class="bulkbar" data-bulkbar role="region" aria-label="Selected devices">
<span class="bulk-count"><strong data-bar-count>3</strong> selected</span>
<button type="button" class="ghost">Clear selection</button>
<button type="submit" name="action" value="OFF">Switch off</button>
<button type="submit" class="primary" name="action" value="ON">Switch on</button></div>
</form>
</main>"""
    page(
        "templates/devices.html",
        "Templates",
        "Device list",
        "27 devices: status, power, uptime, Wi-Fi, row actions, bulk bar",
        body,
        640,
        app=True,
    )

    # ---- device detail
    d = D[0]
    body = f"""{top("devices")}
<main>
<div class="page-head"><h1>Balcony Windowsill {badge("online")}</h1>
<div class="actions"><details class="rowmenu"><summary class="button">More</summary><div class="rowmenu-body"><a href="#">Replace device</a><button type="button">Refresh identity</button></div></details>
<a class="button" href="#">Edit</a></div>
<p class="crumb"><a href="devices.html">Devices</a></p></div>
<div class="jump"><select aria-label="Device pages"><option selected>Overview</option><option>Settings</option><option>Console</option><option>Backups</option></select></div>
<nav class="tabs" aria-label="Device pages"><a href="#" aria-current="page">Overview</a><a href="#">Settings</a><a href="#">Console</a><a href="#">Backups<span class="count">8</span></a></nav>
<div class="cols wide-right">
<div>
<section class="card" aria-labelledby="ident-h"><h2 id="ident-h">Identity</h2>
<dl class="facts">
<dt>Address</dt><dd>{addr(d[5])}</dd><dt>MAC</dt><dd>A4:CF:12:3B:9E:01</dd><dt>Chip family</dt><dd>ESP8266</dd>
<dt>Hardware</dt><dd>ESP8266EX</dd><dt>Firmware</dt><dd>16.0.0 <small>tasmota en</small></dd>
<dt>Flash</dt><dd>4096 KiB, free for OTA 1 412 KiB</dd><dt>MQTT</dt><dd>connected, topic <code>tasmota_3B9E01</code></dd>
<dt>Tags</dt><dd><span class="tag">outdoor</span></dd><dt>Credentials</dt><dd>user admin, password set</dd><dt>OTA</dt><dd>Allowed</dd>
<dt>Identity detected</dt><dd>2026-10-19 21:40:12</dd></dl>
</section>
</div>
<div>
<section class="card" aria-labelledby="health-h"><h2 id="health-h">Health</h2>
<p class="health-line">{badge("online")} via MQTT, last seen <span>8 s ago</span>, up 12d 4h, wifi 78%</p>
<p class="health-line"><small class="muted">Relays as reported by the device:</small> <span class="tag">POWER ON</span> {relay_btns(["ON"])}</p>
<details class="fold"><summary>State changes<span class="meta">4 changes</span></summary>
<table class="dense"><thead><tr><th>When</th><th>Change</th><th>Source</th></tr></thead><tbody>
<tr><td data-label="When" class="s">2026-10-08 05:12:44</td><td data-label="Change" class="p1">{badge("offline")} → {badge("online")}</td><td data-label="Source" class="s">mqtt</td></tr>
</tbody></table></details>
</section>
<section class="card" aria-labelledby="ota-h"><h2 id="ota-h">Transactions</h2>
<table class="dense"><thead><tr><th>Transaction</th><th>State</th><th>Target</th><th>When</th></tr></thead><tbody>
<tr><td data-label="Transaction" class="p1"><a href="#">#41</a></td><td data-label="State" class="p2"><span class="badge tone-ok">Succeeded</span></td><td data-label="Target" class="s">16.0.0</td><td data-label="When" class="s">2026-10-19 21:31</td></tr>
<tr><td data-label="Transaction" class="p1"><a href="#">#33</a></td><td data-label="State" class="p2"><span class="badge tone-ok">Succeeded</span></td><td data-label="Target" class="s">15.1.0</td><td data-label="When" class="s">2026-09-02 20:05</td></tr>
</tbody></table>
</section>
<section id="backups" class="card" aria-labelledby="backups-h"><h2 id="backups-h">Backups</h2>
<div class="toolbar"><button class="primary sm">Back up now</button><a href="#">Compare and restore</a></div>
<table class="dense" data-table="backups"><thead><tr><th>Taken</th><th>Kind</th><th>Firmware</th><th>Size</th><th class="end"><span class="sr-only">Actions</span></th></tr></thead><tbody>
<tr><td data-label="Taken" class="p1">2026-10-20 06:00</td><td data-label="Kind" class="s">Config file</td><td data-label="Firmware" class="s">16.0.0</td><td data-label="Size" class="s">4.1 KiB</td>
<td data-label="Actions" class="p2 end"><div class="row-actions"><a class="button sm" href="#">Restore</a>
<details class="rowmenu"><summary class="button sm icon" aria-label="More actions">{icon("ellipsis")}</summary><div class="rowmenu-body"><a href="#">Download</a><button type="button" class="danger">Delete…</button></div></details></div></td></tr>
<tr><td data-label="Taken" class="p1">2026-10-19 21:30</td><td data-label="Kind" class="s">Snapshot</td><td data-label="Firmware" class="s">15.1.0</td><td data-label="Size" class="s">12.6 KiB</td>
<td data-label="Actions" class="p2 end"><div class="row-actions"><a class="button sm" href="#">Download</a></div></td></tr>
</tbody></table>
</section>
<section class="card danger-zone" aria-labelledby="del-h"><h2 id="del-h">Delete device</h2>
<p class="muted">Removes Balcony Windowsill and its history from tasmofleet. The device itself is not touched.</p>
<button class="danger">Delete device</button></section>
</div>
</div>
</main>"""
    page(
        "templates/device.html",
        "Templates",
        "Device detail",
        "Identity facts, health, transactions, backups with row actions",
        body,
        640,
        app=True,
    )

    # ---- firmware
    rels = [
        ("16.0.0", True, 64, 6, 21, "4 of 5 needed files cached"),
        ("15.1.0", False, 61, 2, 25, "ok"),
        ("15.0.1", False, 61, 1, 26, "none"),
        ("14.6.0", False, 58, 0, 27, "none"),
    ]
    rrows = []
    for n, (v, rec, files, below, _, cov) in enumerate(rels):
        recb = ' <span class="badge accent">recommended</span>' if rec else ""
        fleet = (
            f"{below} to update"
            if below
            else '<span class="muted">none to update</span>'
        )
        if cov == "ok":
            covc = '<span class="badge tone-ok">All 3 needed files cached</span>'
        elif cov == "none":
            covc = '<span class="muted">no device needs a file</span>'
        else:
            covc = f'{cov} <button type="submit" class="sm">Prefetch for my fleet</button><br><small class="error">not offered upstream: tasmota-ir.bin.gz</small>'
        old = ' class="rel-old"' if n >= 3 else ""
        rrows.append(f"""<tr{old}><td data-label="Release" class="p1"><a href="#"><strong>{v}</strong></a>{recb}</td><td data-label="Files" class="s m-hide">{files}</td>
<td data-label="Fleet" class="s">{fleet}</td><td data-label="Coverage" class="m-hide">{covc}</td>
<td class="rowact"><details class="rowmenu"><summary class="button sm">Actions</summary><div class="rowmenu-body"><a href="#">Changelog and fleet</a><button type="button">Prefetch for my fleet</button></div></details></td></tr>""")
    arts = [
        (
            "16.0.0",
            "tasmota.bin.gz",
            "ESP8266 / tasmota",
            "612 KiB",
            "upstream",
            "2 h ago",
            "Balcony Windowsill, Office Desk Lamp, +9",
            "#42",
            True,
            False,
        ),
        (
            "16.0.0",
            "tasmota32.bin",
            "ESP32 / tasmota32",
            "1.9 MiB",
            "upstream",
            "2 h ago",
            "Office Monitor Alice, +7",
            None,
            False,
            True,
        ),
        (
            "15.1.0",
            "tasmota.bin.gz",
            "ESP8266 / tasmota",
            "598 KiB",
            "upstream",
            "48 d ago",
            "Kitchen Counter, TV Backlight, +9",
            None,
            False,
            True,
        ),
        (
            "15.1.0",
            "tasmota-minimal.bin.gz",
            "ESP8266 / minimal",
            "254 KiB",
            "upstream",
            "48 d ago",
            None,
            None,
            False,
            False,
        ),
        (
            "15.0.1",
            "tasmota.bin.gz",
            "ESP8266 / tasmota",
            "596 KiB",
            "upstream",
            "71 d ago",
            "Garden Pump",
            None,
            False,
            False,
        ),
        (
            "16.0.0",
            "tasmota32c3.bin",
            "ESP32-C3 / tasmota32c3",
            "1.8 MiB",
            "upstream",
            "2 h ago",
            "Server Rack Fan",
            None,
            False,
            False,
        ),
    ]
    arows = []
    for i, (rel, fn, chip, size, src, ago, inst, need, inuse, picked) in enumerate(
        arts
    ):
        dis = ' disabled title="Needed by transaction #42"' if inuse else ""
        chk = " checked" if picked else ""
        instc = f'<a href="#">{inst}</a>' if inst else '<span class="muted">none</span>'
        needc = f'<a href="#">{need}</a>' if need else '<span class="muted">none</span>'
        act = (
            '<span class="muted">needed</span>'
            if inuse
            else '<a class="button sm danger" href="#">Delete…</a>'
        )
        sha = [
            "8f704806dbedeaaeca33",
            "ff4f17d21930e36d6d93",
            "1c2b9a0f77e3aa01b2c4",
            "5e9d03b1c6f2a8d4e7f0",
            "0a7c4e9b2d1f3a6c8e5b",
            "c3d2e1f0a9b8c7d6e5f4",
        ][i]
        arows.append(f"""<tr{' class="is-selected"' if picked else ""}><td data-label="Select" class="sel"><label class="hit"><input type="checkbox" aria-label="Select {rel} {fn}"{chk}{dis}></label></td>
<td data-label="Release" class="s">{rel}</td><td data-label="File" class="p1">{fn}</td><td data-label="Chip / variant" class="s">{chip}</td>
<td data-label="Size" class="p2">{size}</td><td data-label="Cached" class="s">{ago}</td><td data-label="Installed on" class="s">{instc}</td>
<td data-label="Needed by" class="s">{needc}</td><td data-label="SHA-256" class="hash m-hide"><code>{sha}</code></td><td data-label="Action" class="end s">{act}</td></tr>""")
    body = f"""{top("firmware")}
<main>
<div class="page-head"><h1>Firmware</h1><div class="actions"><button class="primary">{icon("refresh-cw")}Refresh index</button></div></div>
<section class="card" aria-labelledby="rel-h"><h2 id="rel-h">Upstream releases</h2>
<p class="muted">Last refresh: 2 hours ago.</p>
<p>Recommended release: <strong>16.0.0</strong> (latest stable). 6 of 27 devices run an older version.</p>
<table class="reltable dense"><thead><tr><th>Release</th><th>Files</th><th>Fleet</th><th>Coverage</th><th class="end"><span class="sr-only">Actions</span></th></tr></thead><tbody>
{chr(10).join(rrows)}
</tbody></table></section>
<section class="card" aria-labelledby="custom-h"><details class="m-fold fold-h"><summary><h2 id="custom-h">Custom images</h2></summary><p>…</p></details></section>
<section class="card" aria-labelledby="cache-h"><details class="m-fold fold-h" open><summary><h2 id="cache-h">Cached files</h2></summary>
<p>6 files, 5.8 MiB in total. 4 not needed by any transaction (3.3 MiB).</p>
<form id="fwsel" method="post" action="#">
<div class="toolbar" role="group" aria-label="Selection"><button type="button" class="sm">Select unused</button><button type="button" class="sm">Select none</button><span class="muted" aria-live="polite">2 selected</span></div>
<div class="table-scroll"><table class="dense"><thead><tr><th class="sel"><label class="hit"><input type="checkbox" aria-label="Select all deletable files"></label></th><th>Release</th><th>File</th><th>Chip / variant</th><th>Size</th><th>Cached</th><th>Installed on</th><th>Needed by</th><th>SHA-256</th><th class="end">Action</th></tr></thead><tbody>
{chr(10).join(arows)}
</tbody></table></div>
<div class="bulkbar" data-bulkbar role="region" aria-label="Selected files"><span class="bulk-count"><strong data-bar-count>2</strong> selected</span><button type="button" class="ghost">Clear selection</button><button type="submit" class="danger">Delete selected…</button></div>
</form></details></section>
<section class="card" aria-labelledby="fetch-h"><details class="m-fold fold-h"><summary><h2 id="fetch-h">Prefetch a file</h2></summary><p>…</p></details></section>
</main>"""
    page(
        "templates/firmware.html",
        "Templates",
        "Firmware",
        "Releases with coverage, cached files with selection and bulk delete",
        body,
        640,
        app=True,
    )

    # ---- dashboard
    tx = [
        (
            44,
            "alice",
            "12 min ago",
            7,
            "16.0.0",
            "bad",
            "Failed",
            "0 of 7 updated · 7 offline",
        ),
        (
            43,
            "scheduler",
            "2 h ago",
            1,
            "16.0.0",
            "info",
            "Flashing",
            "Server Rack Fan, step 3 of 5",
        ),
        (42, "alice", "2 h ago", 12, "16.0.0", "ok", "Succeeded", "12 of 12 updated"),
        (
            41,
            "alice",
            "yesterday 21:31",
            5,
            "16.0.0",
            "ok",
            "Succeeded",
            "5 of 5 updated",
        ),
        (
            40,
            "policy",
            "yesterday 18:02",
            3,
            "MQTT settings",
            "warn",
            "Partly done",
            "2 of 3 compliant · 1 deviating",
        ),
        (
            39,
            "alice",
            "2026-10-14",
            1,
            "15.1.0",
            "neutral",
            "Cancelled",
            "cancelled before start",
        ),
    ]
    trows = "\n".join(
        f"""<tr><td data-label="Transaction" class="p1"><a href="#">#{n}</a> <small class="muted">{n_dev} {"device" if n_dev == 1 else "devices"}</small></td>
<td data-label="State" class="p2"><span class="badge tone-{t}">{lab}</span></td><td data-label="Target" class="s">{target}</td>
<td data-label="Result" class="s">{res}</td><td data-label="Started by" class="s">{who}</td><td data-label="Started" class="s">{when}</td></tr>"""
        for n, who, when, n_dev, target, t, lab, res in tx
    )
    stats = "".join(
        f'<li><a class="stat tone-{STATES[s][0]}{" zero" if not COUNT[s] else ""}" href="devices.html"><span class="num">{COUNT[s]}</span>'
        f'<span class="what" data-glyph="{"?" if s == "unknown" else GLYPH[STATES[s][0]]}">{STATES[s][1]}</span></a></li>'
        for s in ("online", "stale", "offline", "auth_failed", "unknown")
    )
    body = f"""{top("dashboard")}
<main>
<div class="page-head"><h1>Dashboard</h1><div class="actions"><a class="button primary" href="#">New transaction</a></div></div>
<p class="warning" role="status">7 devices have been offline for more than a day (Fairylights 1–7). <a href="devices.html">Show them</a></p>
<section class="card" aria-labelledby="fleet-h"><h2 id="fleet-h">Fleet health</h2>
<ul class="stats">{stats}</ul>
<p class="muted summary">27 devices in total · 20 on 16.0.0 · 6 can be updated.</p></section>
<section class="card" aria-labelledby="jobs-h"><h2 id="jobs-h">Recent transactions</h2>
<table class="dense"><thead><tr><th>Transaction</th><th>State</th><th>Target</th><th>Result</th><th>Started by</th><th>Started</th></tr></thead><tbody>
{trows}
</tbody></table>
<p class="summary"><a href="#">All transactions</a></p></section>
</main>"""
    page(
        "templates/dashboard.html",
        "Templates",
        "Dashboard",
        "Stat tiles and recent transactions, one row per transaction",
        body,
        640,
        app=True,
    )


def thumbnail():
    doc = f"""<!doctype html>
<html lang="en" data-theme-auto>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>tasmofleet DS</title>
    <link rel="stylesheet" href="styles.css" />
    <style>
      body {{ margin: 0; }}
      .thumb {{ width: 640px; height: 400px; padding: 28px 32px; display: grid; grid-template-rows: auto auto 1fr auto; gap: 14px; background: var(--color-bg); overflow: hidden; }}
      .thumb h1 {{ font-size: 40px; margin: 0; letter-spacing: -0.03em; border-bottom: 2px solid var(--color-rule); padding-bottom: 10px; }}
      .thumb h1 span {{ color: var(--color-accent); }}
      .thumb .stats {{ margin: 0; }}
    </style>
  </head>
  <body>
    <div class="thumb">
      <h1>tasmofleet<span>.</span>DS</h1>
      <div class="row" style="display:flex;gap:8px;flex-wrap:wrap">{badge("online")}{badge("stale")}{badge("offline")}<span class="badge tone-info">Flashing</span>{badge("unknown")}<span class="badge accent">recommended</span></div>
      <ul class="stats">
        <li><span class="stat tone-ok"><span class="num">{COUNT["online"]}</span><span class="what" data-glyph="✓">Online</span></span></li>
        <li><span class="stat tone-warn"><span class="num">{COUNT["stale"]}</span><span class="what" data-glyph="!">Stale</span></span></li>
        <li><span class="stat tone-bad"><span class="num">{COUNT["offline"]}</span><span class="what" data-glyph="✕">Offline</span></span></li>
      </ul>
      <div style="display:flex;gap:8px;align-items:center"><button class="primary">Add devices</button><button>Refresh index</button>
        <button class="relay-btn on">On</button><button class="relay-btn">Off</button><button class="relay-btn unk">Unknown</button></div>
    </div>
  </body>
</html>
"""
    with open(os.path.join(ROOT, "thumbnail.html"), "w") as f:
        f.write(doc)


if __name__ == "__main__":
    foundations()
    components()
    templates()
    thumbnail()
    print("pages written")
