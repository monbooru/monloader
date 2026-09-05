#!/usr/bin/env bash
# Install the pinned Go toolchain and put it on the job's PATH.

set -euo pipefail

: "${GO_VERSION:?GO_VERSION is not set}"
: "${GO_SHA256_LINUX_AMD64:?GO_SHA256_LINUX_AMD64 is not set}"
: "${GITHUB_PATH:?GITHUB_PATH is not set}"

sudo=""
[ "$(id -u)" -eq 0 ] || sudo=sudo

curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
echo "${GO_SHA256_LINUX_AMD64}  /tmp/go.tgz" | sha256sum -c -
$sudo rm -rf /usr/local/go
$sudo tar -C /usr/local -xzf /tmp/go.tgz
echo /usr/local/go/bin >> "$GITHUB_PATH"
