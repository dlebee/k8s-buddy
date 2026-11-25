#!/usr/bin/env bash
# Builds the DMG: the app next to an Applications shortcut, in a window themed by packaging/macos/dmg
# (regenerate that with packaging/macos/dmg/make-dmg-assets.sh).
# Usage: packaging/macos/build-dmg.sh <path/to/k8s-buddy.app> <out.dmg>
set -euo pipefail

APP="${1:?app}"
OUT="${2:?out dmg}"
THEME="$(cd "$(dirname "$0")/dmg" && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

ditto "$APP" "$STAGE/k8s-buddy.app"
ln -s /Applications "$STAGE/Applications"
mkdir "$STAGE/.background"
cp "$THEME/background.tiff" "$STAGE/.background/background.tiff"
cp "$THEME/DS_Store" "$STAGE/.DS_Store"
# The volume name, the app name and the picture's path are recorded in DS_Store: change one and the
# window loses its picture until make-dmg-assets.sh is run again.
hdiutil create -volname "k8s-buddy" -srcfolder "$STAGE" -fs HFS+ -format UDZO -ov "$OUT"
