"""WCAG contrast check of the token pairs in ds-bundle/styles.css, both themes.

Resolves var() chains and color-mix(in srgb, ...) from the stylesheet itself,
so the table always matches the shipped tokens. Prints a Markdown table and
exits non-zero if any pair is below its threshold.

    python3 -I tools/contrast.py
"""

import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from color import contrast, hex_to_rgb, rgb_to_hex

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CSS = open(os.path.join(ROOT, "ds-bundle", "styles.css")).read()
CSS = re.sub(r"/\*.*?\*/", "", CSS, flags=re.DOTALL)


def decls(block):
    return dict(re.findall(r"(--[\w-]+)\s*:\s*([^;]+);", block))


def root_blocks(css):
    """Top-level ':root { ... }' blocks (light) and the explicit dark block."""
    light, dark = {}, {}
    for m in re.finditer(
        r"(^|\n)(:root(?:\[data-theme=\"dark\"\])?)\s*\{([^{}]*)\}", css
    ):
        (dark if "dark" in m.group(2) else light).update(decls(m.group(3)))
    return light, dark


LIGHT, DARK_ONLY = root_blocks(CSS)
THEMES = {"light": LIGHT, "dark": {**LIGHT, **DARK_ONLY}}


def resolve(val, env, depth=0):
    val = val.strip()
    if depth > 20:
        raise ValueError(val)
    m = re.fullmatch(r"var\((--[\w-]+)\)", val)
    if m:
        return resolve(env[m.group(1)], env, depth + 1)
    m = re.fullmatch(r"color-mix\(in srgb,\s*(.+?)\s+(\d+)%,\s*(.+)\)", val)
    if m:
        a = resolve(m.group(1), env, depth + 1)
        p = int(m.group(2)) / 100
        b = m.group(3).strip()
        if b == "transparent":
            return ("alpha", a, p)
        b = resolve(b, env, depth + 1)
        ra, rb = hex_to_rgb(a), hex_to_rgb(b)
        return rgb_to_hex(tuple(p * x + (1 - p) * y for x, y in zip(ra, rb)))
    if val == "#ffffff" or re.fullmatch(r"#[0-9a-fA-F]{6}", val):
        return val.lower()
    raise ValueError(f"cannot resolve {val!r}")


def col(token, env, under=None):
    v = resolve(env[token], env)
    if isinstance(v, tuple):  # translucent: composite over `under`
        _, a, p = v
        base = col(under, env)
        ra, rb = hex_to_rgb(a), hex_to_rgb(base)
        return rgb_to_hex(tuple(p * x + (1 - p) * y for x, y in zip(ra, rb)))
    return v


# (foreground, background, minimum, what it is)
T, G = 4.5, 3.0
PAIRS = [
    ("--color-text", "--color-bg", T, "body text on page"),
    ("--color-text", "--color-surface", T, "text on card"),
    ("--color-text", "--color-surface-2", T, "text on sunk surface"),
    ("--color-text", "--color-overlay", T, "text in menus/dialogs"),
    ("--color-text", "--color-selected", T, "text in a selected row"),
    ("--color-text-2", "--color-bg", T, "muted text on page"),
    ("--color-text-2", "--color-surface", T, "muted text / placeholder on card"),
    ("--color-text-2", "--color-surface-2", T, "muted text on empty state"),
    ("--color-text-2", "--color-overlay", T, "muted text in overlays"),
    ("--color-text-2", "--color-selected", T, "muted text in a selected row"),
    ("--color-link", "--color-surface", T, "link on card"),
    ("--color-link", "--color-bg", T, "link on page"),
    ("--color-on-accent", "--color-accent", T, "primary button label"),
    ("--color-on-accent", "--color-accent-hover", T, "primary button hover"),
    ("--color-on-accent", "--color-accent-active", T, "primary button active"),
    (
        "--color-accent-soft-text",
        "--color-accent-soft",
        T,
        "accent soft (badge.accent bg)",
    ),
    ("--color-accent-soft-text", "--color-surface", T, "accent text on card"),
    ("--color-on-power", "--color-power-on", T, "power toggle On label"),
    ("--color-danger-text", "--color-surface", T, "danger button label"),
    (
        "--color-danger-text",
        "--color-danger-soft",
        T,
        "danger button hover / error text",
    ),
    ("--color-success-text", "--color-success-soft", T, "success badge / notice"),
    ("--color-warning-text", "--color-warning-soft", T, "warning badge / alert"),
    ("--color-info-text", "--color-info-soft", T, "info badge / alert"),
    ("--color-neutral-text", "--color-neutral-soft", T, "neutral badge (unknown)"),
    ("--color-success-text", "--color-surface", T, "success glyph text on card"),
    ("--color-warning-text", "--color-surface", T, "warning glyph text on card"),
    ("--color-info-text", "--color-surface", T, "info text on card"),
    ("--color-surface", "--color-success", G, "alert glyph on success tone"),
    ("--color-surface", "--color-warning", G, "alert glyph on warning tone"),
    ("--color-surface", "--color-danger", G, "alert glyph on danger tone"),
    ("--color-surface", "--color-info", G, "alert glyph on info tone"),
    ("--color-border", "--color-surface", G, "control border on card"),
    ("--color-border", "--color-bg", G, "control border on page"),
    ("--color-border-overlay", "--color-overlay", G, "overlay border vs overlay"),
    ("--color-border-overlay", "--color-bg", G, "overlay border vs page"),
    ("--color-border-overlay", "--color-surface", G, "overlay border vs card"),
    ("--color-accent", "--color-surface", G, "accent (tab, selection bar) on card"),
    ("--color-accent", "--color-bg", G, "accent on page"),
    ("--color-focus", "--color-surface", G, "focus ring on card"),
    ("--color-focus", "--color-bg", G, "focus ring on page"),
    ("--color-focus", "--color-overlay", G, "focus ring in overlays"),
    ("--color-success", "--color-surface", G, "success tone bar"),
    ("--color-warning", "--color-surface", G, "warning tone bar"),
    ("--color-danger", "--color-surface", G, "danger tone bar"),
    ("--color-info", "--color-surface", G, "info tone bar"),
    ("--color-neutral", "--color-surface", G, "neutral tone bar"),
    ("--color-danger-border", "--color-surface", G, "danger button border"),
    ("--color-power-on", "--color-surface", G, "power On fill vs card"),
    ("--color-rule", "--color-surface", G, "2px section rule"),
]


# Machado et al. 2009, severity 1.0, applied in linear sRGB
CVD = {
    "normal": None,
    "protan": ((0.152286, 1.052583, -0.204868), (0.114503, 0.786281, 0.099216), (-0.003882, -0.048116, 1.051998)),
    "deutan": ((0.367322, 0.860646, -0.227968), (0.280085, 0.672501, 0.047413), (-0.011820, 0.042940, 0.968881)),
    "tritan": ((1.255528, -0.076749, -0.178779), (-0.078411, 0.930809, 0.147602), (0.004733, 0.691367, 0.303900)),
}
ROLES = ["--color-accent", "--color-success", "--color-warning", "--color-danger", "--color-info", "--color-neutral"]


def oklab(hexv, m):
    from color import _gam, _lin, rgb_to_oklch
    import math
    lin = [_lin(c) for c in hex_to_rgb(hexv)]
    if m:
        lin = [max(0.0, min(1.0, sum(m[i][j] * lin[j] for j in range(3)))) for i in range(3)]
    L, C, H = rgb_to_oklch(tuple(_gam(c) for c in lin))
    return L, C * math.cos(math.radians(H)), C * math.sin(math.radians(H))


def cvd_table():
    """Smallest OKLab distance between any two role colours (solid tones)."""
    print("\n| Theme | Vision | Closest pair | Delta E (OKLab x100) |")
    print("|---|---|---|---|")
    for name, env in THEMES.items():
        for cond, m in CVD.items():
            labs = {r: oklab(col(r, env), m) for r in ROLES}
            best = None
            for i, a in enumerate(ROLES):
                for b in ROLES[i + 1:]:
                    d = sum((x - y) ** 2 for x, y in zip(labs[a], labs[b])) ** 0.5 * 100
                    if best is None or d < best[0]:
                        best = (d, a, b)
            d, a, b = best
            short = lambda t: t.replace("--color-", "")
            print(f"| {name} | {cond} | {short(a)} / {short(b)} | {d:.1f} |")


def main():
    rows, bad = [], 0
    for fg, bg, need, what in PAIRS:
        cells = []
        for name, env in THEMES.items():
            b = col(bg, env, "--color-surface")
            f = col(fg, env, bg)
            r = contrast(f, b)
            ok = r >= need
            bad += not ok
            cells.append(f"{r:.2f} {'pass' if ok else 'FAIL'}")
        rows.append(
            f"| {what} | `{fg}` on `{bg}` | {need:g}:1 | {cells[0]} | {cells[1]} |"
        )
    print("| Pair | Tokens | Min | Light | Dark |")
    print("|---|---|---|---|---|")
    print("\n".join(rows))
    print(f"\n{len(PAIRS) * 2 - bad} of {len(PAIRS) * 2} checks pass.")
    cvd_table()
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
