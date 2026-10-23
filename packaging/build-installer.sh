#!/bin/sh
# packaging/build-installer.sh <lite|bundled>
#
# Builds the Windows installer from the payload build-binaries.sh staged and writes it to dist/.
set -eu

shape=${1:?usage: build-installer.sh lite|bundled}
. ./packaging/release-env.sh
payload=packaging/windows/payload
staged="stage/${APP}_${VERSION#v}_portable_${shape}_windows_amd64-setup"

[ -d "$staged" ] || { echo "no staged payload at $staged" >&2; exit 1; }
rm -rf "$payload"

cp -r "$staged" "$payload"

mkdir -p dist

image=amake/innosetup:latest@sha256:e003376ba818547275fe10c95e2a29be0f2d12d45e9eb8f205b6672dc5685bb1
cid=$(docker create -w /work "$image" "/DMyAppVersion=${VERSION#v}" "/DMyAppTools=$shape" "packaging/windows/$APP.iss")
stage=$(mktemp -d)
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true; rm -rf "$stage"' EXIT

mkdir -m 0777 "$stage/dist"
docker cp packaging "$cid:/work/packaging"
docker cp "$stage/dist" "$cid:/work/dist"
docker start -a "$cid"

status=$(docker wait "$cid")
[ "$status" = 0 ] || { echo "ISCC exited with status $status" >&2; exit 1; }
docker cp "$cid:/work/dist/." dist/
ls -la dist
