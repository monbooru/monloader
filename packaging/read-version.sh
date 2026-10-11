#!/bin/sh
# Hand VERSION.md to the job as the step output VERSION, refusing anything
# but strict SemVer.

set -eu

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"

VERSION=$(tr -d '[:space:]' < VERSION.md)
if ! echo "$VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "VERSION.md must be strict SemVer 'vMAJOR.MINOR.PATCH', got: $VERSION" >&2
  exit 1
fi
echo "VERSION=$VERSION" >> "$GITHUB_OUTPUT"
