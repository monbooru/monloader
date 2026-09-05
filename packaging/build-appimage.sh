#!/bin/sh
# packaging/build-appimage.sh
#
# Builds the bundled AppImage into dist/
set -eu

. ./packaging/release-env.sh

tool_version=1.9.1
tool_sha=ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0
runtime_version=20251108
runtime_sha=2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d
arch=x86_64

fetch() {
  curl -fsSL -o "$1" "$2"
  echo "$3  $1" | sha256sum -c -
}

work=stage/appimage
mkdir -p dist "$work"
fetch "$work/appimagetool" \
  "https://github.com/AppImage/appimagetool/releases/download/${tool_version}/appimagetool-${arch}.AppImage" \
  "$tool_sha"
chmod 0755 "$work/appimagetool"
fetch "$work/runtime" \
  "https://github.com/AppImage/type2-runtime/releases/download/${runtime_version}/runtime-${arch}" \
  "$runtime_sha"

app=AppDir
rm -rf "$app"
mkdir -p "$app/usr/bin" "$app/usr/share/applications"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w $LDFLAGS -X '$MOD.Package=appimage' -X 'main.defaultDesktop=true'" \
  -o "$app/usr/bin/monloader" ./cmd/monloader

cp tools/gallery-dl "$app/usr/bin/"
cp LICENSE README.md "$app/"
cp tools/LICENSE.gallery-dl "$app/"
printf 'gallery-dl %s, from https://codeberg.org/mikf/gallery-dl/releases/tag/v%s\nLicensed GPL-2.0-only; see LICENSE.gallery-dl. Source is at that release.\n' \
  "$GDL_VERSION" "$GDL_VERSION" > "$app/README.gallery-dl"
install -m 0755 packaging/appimage/AppRun "$app/AppRun"
cp packaging/monloader.desktop "$app/monloader.desktop"
cp packaging/monloader.desktop "$app/usr/share/applications/monloader.desktop"
cp packaging/icons/monloader-256.png "$app/monloader.png"
for size in 48 64 128 256; do
  install -Dm644 "packaging/icons/monloader-$size.png" \
    "$app/usr/share/icons/hicolor/${size}x${size}/apps/monloader.png"
done

out="dist/monloader_${VERSION#v}_${arch}.AppImage"
rm -f "$out"

ARCH=$arch "./$work/appimagetool" --appimage-extract-and-run \
  --runtime-file "$work/runtime" "$app" "$out"
rm -rf "$app"
ls -la dist
