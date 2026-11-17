#!/usr/bin/env bash
# Regenerates the app icons (needs rsvg-convert: `brew install librsvg`, plus iconutil and python3):
#   app-icon.svg       → packaging/macos/AppIcon.icns (macOS: the buddy on a rounded tile)
#   app-icon-small.svg → packaging/windows/k8s-buddy.ico (Windows: the buddy alone, readable at 16 px)
#                        and cmd/k8s-buddy/rsrc_windows_{amd64,arm64}.syso (the icon inside the .exe)
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

ICO="$(mktemp -d)"
pngs=()
for s in 16 20 24 32 40 48 64 256; do
	rsvg-convert -w "$s" -h "$s" app-icon-small.svg -o "$ICO/$s.png"
	pngs+=("$ICO/$s.png")
done
python3 make-ico.py ../windows/k8s-buddy.ico "${pngs[@]}"
# The .exe icon: Windows resource objects that `go build` links into Windows builds.
for arch in amd64 arm64; do
	python3 make-syso.py ../windows/k8s-buddy.ico "$arch" "../../cmd/k8s-buddy/rsrc_windows_$arch.syso"
done
echo "wrote packaging/macos/AppIcon.icns, packaging/windows/k8s-buddy.ico, cmd/k8s-buddy/rsrc_windows_*.syso and packaging/icon/app-icon-1024.png"
