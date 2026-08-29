#!/bin/sh
# packaging/fetch-tools.sh
#
# Downloads the pinned one-file gallery-dl builds into tools/, where
# build-binaries.sh expects them. 
set -eu

base="https://codeberg.org/mikf/gallery-dl/releases/download/v${GDL_VERSION}"
mkdir -p tools
curl -fsSL -o tools/gallery-dl "$base/gallery-dl.bin"
echo "${GDL_SHA256_LINUX_AMD64}  tools/gallery-dl" | sha256sum -c -
chmod 0755 tools/gallery-dl
curl -fsSL -o tools/gallery-dl.exe "$base/gallery-dl.exe"
echo "${GDL_SHA256_WINDOWS_AMD64}  tools/gallery-dl.exe" | sha256sum -c -
curl -fsSL -o tools/LICENSE.gallery-dl \
  "https://codeberg.org/mikf/gallery-dl/raw/tag/v${GDL_VERSION}/LICENSE"
ls -la tools
