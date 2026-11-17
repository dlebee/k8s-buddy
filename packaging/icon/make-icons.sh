#!/usr/bin/env bash
# Regenerates the app icons from app-icon.svg (needs rsvg-convert: `brew install librsvg`).
# The results are committed, so release builds don't need this.
set -euo pipefail
cd "$(dirname "$0")"
SET="$(mktemp -d)/AppIcon.iconset"
mkdir -p "$SET"
for s in 16 32 128 256 512; do
	rsvg-convert -w "$s" -h "$s" app-icon.svg -o "$SET/icon_${s}x${s}.png"
	rsvg-convert -w $((s * 2)) -h $((s * 2)) app-icon.svg -o "$SET/icon_${s}x${s}@2x.png"
done
iconutil -c icns "$SET" -o ../macos/AppIcon.icns
rsvg-convert -w 1024 -h 1024 app-icon.svg -o app-icon-1024.png
echo "wrote packaging/macos/AppIcon.icns and packaging/icon/app-icon-1024.png"
