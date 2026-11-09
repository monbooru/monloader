#!/bin/sh
set -eu

desktop=${1:?usage: packaging/metainfo.sh <desktop-id>}
. ./packaging/release-env.sh
src=packaging/flatpak/$APPID.metainfo.xml
heading=$(awk '/^## /{print $2; exit}' CHANGELOG.md)
date=$(awk '/^## /{print $4; exit}' CHANGELOG.md)
[ "$heading" = "[$VERSION]" ] || {
  echo "CHANGELOG.md's newest entry is $heading, VERSION.md says $VERSION" >&2
  exit 1
}
grep -qF '<!--RELEASE-->' "$src" || {
  echo "the metainfo has no <!--RELEASE--> marker to fill" >&2
  exit 1
}
sed -e "s|<!--RELEASE-->|<release version=\"${VERSION#v}\" date=\"$date\"/>|" \
  -e "s|<launchable type=\"desktop-id\">[^<]*</launchable>|<launchable type=\"desktop-id\">$desktop</launchable>|" \
  "$src"
