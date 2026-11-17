#!/usr/bin/env bash
# Builds a universal (Apple Silicon + Intel) k8s-buddy.app.
# Usage: packaging/macos/build-app.sh <version> <out-dir>
set -euo pipefail

VERSION="${1:?version}"
OUT="${2:?out dir}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
APP="$OUT/k8s-buddy.app"
BUILD="$(mktemp -d)"
trap 'rm -rf "$BUILD"' EXIT

export MACOSX_DEPLOYMENT_TARGET=13.0
LDFLAGS="-s -w -X main.version=$VERSION"

for arch in arm64 amd64; do
	clang_arch="$arch"
	[ "$arch" = amd64 ] && clang_arch=x86_64
	echo "building darwin/$arch"
	(cd "$ROOT" && CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" \
		CGO_CFLAGS="-arch $clang_arch -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
		CGO_LDFLAGS="-arch $clang_arch -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
		go build -trimpath -ldflags "$LDFLAGS" -o "$BUILD/k8s-buddy-$arch" ./cmd/k8s-buddy)
done

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$ROOT/packaging/macos/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"
lipo -create -output "$APP/Contents/MacOS/k8s-buddy" "$BUILD/k8s-buddy-arm64" "$BUILD/k8s-buddy-amd64"
# Bundle versions must be numeric (1.2.3); the full version stays in the binary.
PLIST_VERSION="$(echo "$VERSION" | sed -E 's/^([0-9]+\.[0-9]+(\.[0-9]+)?)([^0-9.].*)?$/\1/; /^[0-9]+\.[0-9]+(\.[0-9]+)?$/!s/.*/0.0.0/')"
sed "s/VERSION/$PLIST_VERSION/g" "$ROOT/packaging/macos/Info.plist" > "$APP/Contents/Info.plist"
lipo -info "$APP/Contents/MacOS/k8s-buddy"
echo "built $APP"
