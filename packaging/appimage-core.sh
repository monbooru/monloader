# Sourced by build-appimage.sh after release-env.sh.

tool_version=1.9.1
runtime_version=20251108
case "${GOARCH:?GOARCH must name the target}" in
amd64)
  arch=x86_64
  tool_sha=ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0
  runtime_sha=2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d
  ;;
arm64)
  arch=aarch64
  tool_sha=f0837e7448a0c1e4e650a93bb3e85802546e60654ef287576f46c71c126a9158
  runtime_sha=00cbdfcf917cc6c0ff6d3347d59e0ca1f7f45a6df1a428a0d6d8a78664d87444
  ;;
*) echo "no AppImage runtime published for $GOARCH" >&2; exit 1 ;;
esac

work=stage/appimage
app=AppDir

fetch() {
  curl -fsSL -o "$1" "$2"
  echo "$3  $1" | sha256sum -c -
}

appimage_begin() {
  mkdir -p dist "$work"
  fetch "$work/appimagetool" \
    "https://github.com/AppImage/appimagetool/releases/download/${tool_version}/appimagetool-${arch}.AppImage" \
    "$tool_sha"
  chmod 0755 "$work/appimagetool"
  fetch "$work/runtime" \
    "https://github.com/AppImage/type2-runtime/releases/download/${runtime_version}/runtime-${arch}" \
    "$runtime_sha"
  rm -rf "$app"
  mkdir -p "$app/usr/bin" "$app/usr/share/applications"
}

appimage_finish() {
  cp LICENSE README.md "$app/"
  install -m 0755 packaging/appimage/AppRun "$app/AppRun"
  cp "packaging/$APP.desktop" "$app/$APP.desktop"
  cp "packaging/$APP.desktop" "$app/usr/share/applications/$APP.desktop"
  cp "packaging/icons/$APP-256.png" "$app/$APP.png"
  for size in 48 64 128 256; do
    install -Dm644 "packaging/icons/$APP-$size.png" \
      "$app/usr/share/icons/hicolor/${size}x${size}/apps/$APP.png"
  done

  out="dist/${APP}_${VERSION#v}_${arch}.AppImage"
  rm -f "$out"
  ARCH=$arch "./$work/appimagetool" --appimage-extract-and-run \
    --runtime-file "$work/runtime" "$app" "$out"
  rm -rf "$app"
  ls -la dist
}
