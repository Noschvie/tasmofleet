"""Print the 100-900 OKLCH ramps of styles.css as CSS custom properties.

All ramps share one lightness scale, so step N of any ramp has the same
perceived lightness; the two brand teals are pinned exactly (accent-400 is
the dark-theme accent #2dd4bf, accent-700 the light-theme accent #0f766e).

    python3 -I tools/ramps.py > /tmp/ramps.css
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from color import oklch_hex

L = {
    100: 0.97,
    200: 0.925,
    300: 0.86,
    400: 0.785,
    500: 0.69,
    600: 0.6,
    700: 0.511,
    800: 0.42,
    900: 0.32,
    950: 0.245,
}
# chroma envelope (fraction of the ramp's peak chroma) per step
ENV = {
    100: 0.18,
    200: 0.38,
    300: 0.7,
    400: 1.0,
    500: 1.0,
    600: 0.92,
    700: 0.8,
    800: 0.66,
    900: 0.5,
    950: 0.38,
}

RAMPS = {
    # name: (hue at 100, hue at 900, peak chroma)
    "accent": (181.0, 187.0, 0.133),
    "success": (150.0, 152.0, 0.17),
    "warning": (75.0, 58.0, 0.165),
    "danger": (25.0, 27.0, 0.2),
    "info": (255.0, 262.0, 0.18),
    "neutral": (190.0, 190.0, 0.012),
}
# neutral: almost grey at the light end, a trace of the teal hue in the darks
NEUTRAL_C = {100: 0.004, 200: 0.006, 300: 0.008, 400: 0.01, 500: 0.011, 600: 0.012, 700: 0.012, 800: 0.012, 900: 0.012, 950: 0.012}
PINNED = {("accent", 400): "#2dd4bf", ("accent", 700): "#0f766e"}

for name, (h0, h1, peak) in RAMPS.items():
    print(f"  /* {name}: OKLCH, hue {h0:g}->{h1:g}, peak chroma {peak:g} */")
    steps = sorted(L)
    for i, step in enumerate(steps):
        hue = h0 + (h1 - h0) * i / (len(steps) - 1)
        c = NEUTRAL_C[step] if name == "neutral" else peak * ENV[step]
        hx, c_used = oklch_hex(L[step], c, hue)
        hx = PINNED.get((name, step), hx)
        print(
            f"  --color-{name}-{step}: {hx}; /* oklch({L[step]:.3f} {c_used:.3f} {hue:.0f}) */"
        )

# theme grounds that sit outside the ramp (one lightness each)
print("  /* grounds (OKLCH, hue 190) */")
for name, l, c in [("light-bg", 0.972, 0.003), ("light-surface-2", 0.952, 0.004),
                   ("dark-bg", 0.17, 0.008), ("dark-surface", 0.205, 0.009),
                   ("dark-surface-2", 0.235, 0.01), ("dark-overlay", 0.275, 0.011),
                   ("dark-text", 0.945, 0.006), ("dark-text-2", 0.775, 0.012),
                   ("dark-divider", 0.34, 0.012), ("dark-border", 0.56, 0.012),
                   ("light-text", 0.225, 0.012)]:
    hx, _ = oklch_hex(l, c, 190)
    print(f"  --{name}: {hx}; /* oklch({l:.3f} {c:.3f} 190) */")
