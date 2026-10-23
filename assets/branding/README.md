# Branding

The TasmoFleet logo is the mark of the design v2 prototype's top bar: a white ring on a blue
tile (`#1f63a8`), the same in light and dark themes. Licensed under AGPL-3.0-or-later like the rest of the
repository. It shares no shape or colour scheme with the Tasmota logo; TasmoFleet is not
affiliated with Tasmota.

| File | Use |
| ---- | --- |
| `logo.svg` | The mark (source of every other file) |
| `wordmark.svg`, `wordmark-dark.svg` | Mark + name, for light / dark backgrounds (README header) |
| `favicon.svg`, `favicon.ico` | Browser tab icon (ICO 16/32/48) |
| `apple-touch-icon.png` | 180x180, full-bleed (iOS rounds it) |
| `icon-192.png`, `icon-512.png` | Web manifest, Unraid `<Icon>`, Authentik application icon |
| `addon-icon.png`, `addon-logo.png` | Home Assistant add-on `icon.png` (128x128) and `logo.png` (250x100) |

`design/tools/brand-icons.sh` regenerates every raster from `logo.svg` (and the add-on logo from
`wordmark.svg`), copies the files the web UI serves to `internal/web/static/` (a test checks they
stay identical) and updates `internal/web/static/SHA256SUMS`. The script needs `rsvg-convert` and
ImageMagick 7; a rerun produces the same bytes.
