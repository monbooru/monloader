#!/bin/sh
# Installs the runtime and builds the Flatpak bundle into dist/.

set -eu

. ./packaging/release-env.sh
app=io.github.monbooru.$(echo "$APP" | awk '{print toupper(substr($0, 1, 1)) substr($0, 2)}')

runtime=$(awk -F"'" '/^runtime-version:/ {print $2}' "packaging/flatpak/$app.yml")


if ! err=$(bwrap --ro-bind / / --unshare-user --unshare-pid --proc /proc true 2>&1); then
  {
    echo "$err"
    if bwrap --ro-bind / / --unshare-user true 2>/dev/null; then
      echo "bwrap cannot mount /proc here, so flatpak-builder cannot build a module."
      echo "This job's container has a masked /proc, and nothing in this script"
      echo "can unmask it. The runner has to start the job's container"
      echo "privileged, or the job has to run outside a container."
    else
      echo "bwrap cannot open a user namespace here, so flatpak-builder cannot"
      echo "build a module. Ubuntu 24.04 and newer refuse one to any binary no"
      echo "AppArmor profile covers, and bubblewrap ships none. The job has to"
      echo "set kernel.apparmor_restrict_unprivileged_userns=0, or load"
      echo "/usr/share/apparmor/extra-profiles/bwrap-userns-restrict."
    fi
  } >&2
  exit 1
fi

fbver=$(flatpak-builder --version | awk '{print $NF}')
fbrest=${fbver#*.}
if [ "${fbver%%.*}" -lt 1 ] || { [ "${fbver%%.*}" -eq 1 ] && [ "${fbrest%%.*}" -lt 4 ]; }; then
  echo "flatpak-builder $fbver is too old for the $runtime runtime; 1.4 or newer" >&2
  exit 1
fi

flatpak remote-add --user --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo

cfg="$HOME/.local/share/flatpak/repo/config"
if [ -f "$cfg" ]; then
  sed -i '/^\[remote "flathub"\]/a http2=false' "$cfg"
fi

ok=0
for attempt in 1 2 3; do
  if flatpak install --user -y --noninteractive flathub \
       "org.freedesktop.Platform//$runtime" "org.freedesktop.Sdk//$runtime" \
       "org.freedesktop.Sdk.Extension.golang//$runtime"; then
    ok=1
    break
  fi
  echo "flathub did not answer (attempt $attempt of 3); retrying"
  sleep 60
done
[ "$ok" = 1 ] || { echo "flathub is not answering" >&2; exit 1; }

mkdir -p dist
flatpak-builder --user --disable-rofiles-fuse --force-clean --repo=repo \
  build-dir "packaging/flatpak/$app.yml"

flatpak build-bundle --runtime-repo=https://flathub.org/repo/flathub.flatpakrepo \
  repo "dist/${APP}_${VERSION#v}_x86_64.flatpak" "$app"
ls -la dist
