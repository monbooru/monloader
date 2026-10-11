#!/bin/sh
# Stamps packaging/aur/PKGBUILD with this version and the tag tarball's
# checksum, and writes it with a generated .SRCINFO into dist/aur/.
set -eu

. ./packaging/release-env.sh
pkgver=${VERSION#v}
tarball="$REPO/archive/refs/tags/$VERSION.tar.gz"

out=dist/aur
rm -rf "$out"
mkdir -p "$out"

sha=$(curl -fsSL "$tarball" | sha256sum | cut -d' ' -f1)
sed -E "s|^pkgver=.*|pkgver=$pkgver|; s|^sha256sums=\('[0-9a-f]{64}'\)|sha256sums=('$sha')|" \
  packaging/aur/PKGBUILD > "$out/PKGBUILD"
grep -qx "pkgver=$pkgver" "$out/PKGBUILD" || { echo "no pkgver line to stamp" >&2; exit 1; }
grep -qx "sha256sums=('$sha')" "$out/PKGBUILD" || { echo "no sha256sums line to stamp" >&2; exit 1; }
cp packaging/aur/LICENSE "$out/LICENSE"

image=archlinux:base-devel@sha256:68bfc3b0d277b08a99101dc9b94aaa03e5ae70cf1b4fb965c03b2b87b915760d
cid=$(docker create -w /pkg "$image" sh -c \
  'useradd -m build && chown -R build /pkg && runuser -u build -- makepkg --printsrcinfo > .SRCINFO')
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
docker cp "$out/." "$cid:/pkg"
docker start -a "$cid"
status=$(docker wait "$cid")
[ "$status" = 0 ] || { echo "makepkg --printsrcinfo exited with status $status" >&2; exit 1; }
docker cp "$cid:/pkg/.SRCINFO" "$out/.SRCINFO"

ls -la "$out"
