#!/bin/sh
# packaging/install-go.sh <version> <sha256> [arch]
#
# Installs the pinned Go toolchain into /usr/local/go and puts it on the
# job's PATH. 
set -eu

version=$1
sha=$2
arch=${3:-amd64}

as_root() {
  if [ "$(id -u)" = 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${version}.linux-${arch}.tar.gz"
echo "${sha}  /tmp/go.tgz" | sha256sum -c -
as_root rm -rf /usr/local/go
as_root tar -C /usr/local -xzf /tmp/go.tgz
rm -f /tmp/go.tgz
echo /usr/local/go/bin >> "$GITHUB_PATH"
