#!/bin/sh
# Regenerates every TasmoFleet icon from the one mark, assets/branding/logo.svg (design v2, D16:
# the prototype's top-bar mark, a white ring on a blue tile). Needs rsvg-convert (librsvg) and
# ImageMagick 7 (magick). Run from anywhere; writes assets/branding/ and the copies the web UI
# serves from internal/web/static/ (a Go test keeps the two identical), then the checksums.
set -eu
cd "$(dirname "$0")/../.."
b=assets/branding
s=internal/web/static
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# full-bleed variant for iOS, which rounds the corners itself
sed 's/ rx="8"//' "$b/logo.svg" >"$tmp/fullbleed.svg"

png() { # png <svg> <size> <out>: PNG without time chunks, so a rerun gives the same bytes
	rsvg-convert -w "$2" -h "$2" "$1" -o "$tmp/raw.png"
	magick "$tmp/raw.png" -define png:exclude-chunks=date,time "$3"
}

cp "$b/logo.svg" "$b/favicon.svg"
png "$b/logo.svg" 192 "$b/icon-192.png"
png "$b/logo.svg" 512 "$b/icon-512.png"
png "$b/logo.svg" 128 "$b/addon-icon.png"
png "$tmp/fullbleed.svg" 180 "$b/apple-touch-icon.png"
for n in 16 32 48; do rsvg-convert -w $n -h $n "$b/logo.svg" -o "$tmp/ico-$n.png"; done
magick "$tmp/ico-16.png" "$tmp/ico-32.png" "$tmp/ico-48.png" "$b/favicon.ico"
# Home Assistant add-on logo: the wordmark at 250x100
rsvg-convert -w 250 -h 100 --keep-aspect-ratio "$b/wordmark.svg" -o "$tmp/raw.png"
magick "$tmp/raw.png" -gravity center -background none -extent 250x100 -define png:exclude-chunks=date,time "$b/addon-logo.png"

for f in favicon.svg favicon.ico apple-touch-icon.png icon-192.png icon-512.png; do
	cp "$b/$f" "$s/$f"
done
(cd "$s" && sha256sum htmx-2.0.11.min.js htmx-ext-sse-2.2.4.js geist-latin-ext.woff2 geist-latin.woff2 \
	geist-mono-latin-ext.woff2 geist-mono-latin.woff2 geist-OFL.txt \
	favicon.svg favicon.ico apple-touch-icon.png icon-192.png icon-512.png >SHA256SUMS)
echo "icons regenerated from $b/logo.svg"
