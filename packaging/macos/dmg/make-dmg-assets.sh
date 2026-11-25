#!/usr/bin/env bash
# Regenerates the look of the DMG window (needs rsvg-convert: `brew install librsvg`, plus uv, which
# fetches the two Python packages make-dsstore.py asks for):
#   background.svg  → background.tiff (the picture behind the icons, 1x and 2x in one file)
#   make-dsstore.py → DS_Store        (window size, icon positions and the link to the picture)
# The results are committed, so release builds don't need this. build-dmg.sh puts them in the DMG.
set -euo pipefail
cd "$(dirname "$0")"

VOL="/Volumes/k8s-buddy"
[ ! -e "$VOL" ] || { echo "$VOL is mounted: eject it first" >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'hdiutil detach "$VOL" >/dev/null 2>&1 || true; rm -rf "$TMP"' EXIT

rsvg-convert -w 660 -h 400 background.svg -o "$TMP/bg.png"
rsvg-convert -w 1320 -h 800 background.svg -o "$TMP/bg@2x.png"
tiffutil -cathidpicheck "$TMP/bg.png" "$TMP/bg@2x.png" -out "$TMP/bg.tiff"
tiffutil -lzw "$TMP/bg.tiff" -out background.tiff

# Finder finds the picture through an alias (volume name + path), so the layout is written on a scratch
# volume that has the same name and the same files as the released one.
mkdir -p "$TMP/stage/.background" "$TMP/stage/k8s-buddy.app"
cp background.tiff "$TMP/stage/.background/background.tiff"
ln -s /Applications "$TMP/stage/Applications"
hdiutil create -volname k8s-buddy -srcfolder "$TMP/stage" -fs HFS+ -format UDRW -ov "$TMP/scratch.dmg" >/dev/null
hdiutil attach -nobrowse -noautoopen "$TMP/scratch.dmg" >/dev/null
uv run make-dsstore.py "$VOL"
cp "$VOL/.DS_Store" DS_Store
echo "wrote packaging/macos/dmg/background.tiff and packaging/macos/dmg/DS_Store"
