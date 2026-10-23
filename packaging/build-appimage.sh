#!/bin/sh
# Builds the bundled AppImage for $GOARCH (amd64 when unset) into dist/.
set -eu

GOARCH=${GOARCH:-amd64}
. ./packaging/release-env.sh
. ./packaging/appimage-core.sh

appimage_begin

GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w $LDFLAGS -X '$MOD.Package=appimage' -X 'main.defaultDesktop=true'" \
  -o "$app/usr/bin/monloader" ./cmd/monloader

cp tools/gallery-dl "$app/usr/bin/"
cp tools/LICENSE.gallery-dl "$app/"
printf 'gallery-dl %s, from https://codeberg.org/mikf/gallery-dl/releases/tag/v%s\nLicensed GPL-2.0-only; see LICENSE.gallery-dl. Source is at that release.\n' \
  "$GDL_VERSION" "$GDL_VERSION" > "$app/README.gallery-dl"

appimage_finish
