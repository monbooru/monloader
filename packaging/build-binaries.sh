#!/bin/sh
# packaging/build-binaries.sh
# Assembles every downloadable archive into dist/. 
set -eu

. ./packaging/release-env.sh
mkdir -p dist stage

compile() {
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w $gui $LDFLAGS -X '$MOD.Package=$1' -X 'main.defaultDesktop=$desktop'" \
    -o "$2" ./cmd/monloader
}

for target in linux/amd64 linux/arm64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  exe=monloader
  pkg=tarball
  if [ "$os" = windows ]; then
    exe=monloader.exe
    pkg=zip
  fi
  for shape in lite bundled; do
    tool=""
    if [ "$shape" = bundled ]; then
      if [ "$os" = windows ]; then
        tool=tools/gallery-dl.exe
      elif [ "$arch" = amd64 ]; then
        tool=tools/gallery-dl
      else
        continue
      fi
    fi
    for profile in desktop server; do
      gui=""
      desktop=false
      if [ "$profile" = desktop ]; then
        desktop=true
        if [ "$os" = windows ]; then
          gui="-H windowsgui"
        fi
      fi
      name="monloader_${VERSION#v}_${profile}_${shape}_${os}_${arch}"
      dir="stage/$name"
      rm -rf "$dir"
      mkdir -p "$dir"
      compile "$pkg" "$dir/$exe"
      cp LICENSE README.md "$dir/"
      if [ -n "$tool" ]; then
        cp "$tool" "$dir/"
        cp tools/LICENSE.gallery-dl "$dir/"
        printf 'gallery-dl %s, from https://codeberg.org/mikf/gallery-dl/releases/tag/v%s\nLicensed GPL-2.0-only; see LICENSE.gallery-dl. Source is at that release.\n' \
          "$GDL_VERSION" "$GDL_VERSION" > "$dir/README.gallery-dl"
      fi
      if [ "$os" = windows ]; then
        cp packaging/icons/monloader.ico "$dir/"
        (cd stage && zip -qr "../dist/$name.zip" "$name")
        if [ "$profile" = desktop ]; then
          rm -rf "$dir-setup"
          cp -r "$dir" "$dir-setup"
          compile installer "$dir-setup/$exe"
        fi
      else
        tar -czf "dist/$name.tar.gz" -C stage "$name"
      fi
    done
  done
done
ls -la dist
