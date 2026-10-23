"""Write ds-bundle/theme.json: the resolved semantic tokens of both themes.

python3 -I tools/theme_json.py
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import contrast as c

SEMANTIC = [
    k
    for k in c.LIGHT
    if k.startswith("--color-")
    and not k[8:].split("-")[0]
    in {"accent", "success", "warning", "danger", "info", "neutral"}
]
SEMANTIC += [
    k
    for k in c.LIGHT
    if k.startswith("--color-") and k not in SEMANTIC and not k[-3:].isdigit()
]


def value(token, env):
    try:
        v = c.resolve(env[token], env)
    except ValueError:
        return env[token].strip()
    if isinstance(v, tuple):
        _, hexv, p = v
        return f"{hexv} @ {round(p * 100)}%"
    return v


def main():
    ramps = {}
    for k, v in c.LIGHT.items():
        if k.startswith("--color-") and k[-3:].isdigit():
            name, step = k[8:].rsplit("-", 1)
            ramps.setdefault(name, {})[step] = v.strip()
    themes = {
        name: {k: value(k, env) for k in sorted(set(SEMANTIC))}
        for name, env in c.THEMES.items()
    }
    other = {
        k: c.LIGHT[k].strip()
        for k in c.LIGHT
        if k.startswith(
            (
                "--font",
                "--fs-",
                "--lh-",
                "--space-",
                "--radius",
                "--rule-w",
                "--hair-w",
                "--control-",
                "--grid-",
                "--gutter",
                "--container",
                "--page-pad",
                "--z-",
            )
        )
    }
    data = {
        "name": "tasmofleet DS",
        "basedOn": "Modernist (Claude Design)",
        "description": "Flat, 0 radius, 2px section rules, modular grid, Archivo; ink on ground, "
        "teal accent used sparingly; light and dark with the same token names.",
        "css": "styles.css",
        "themeSelection": {
            "default": "prefers-color-scheme",
            "override": 'data-theme="light" | "dark" on <html>',
        },
        "fonts": [
            {
                "family": "Archivo",
                "weights": "400-800 (variable)",
                "files": ["fonts/archivo-latin.woff2", "fonts/archivo-latin-ext.woff2"],
                "license": "fonts/OFL.txt",
            }
        ],
        "ramps": ramps,
        "themes": themes,
        "tokens": other,
    }
    path = os.path.join(c.ROOT, "ds-bundle", "theme.json")
    with open(path, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    print("wrote", path)


if __name__ == "__main__":
    main()
