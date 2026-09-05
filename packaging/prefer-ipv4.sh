#!/usr/bin/env bash
# Probe the route to flathub and, where the runner advertises ipv6 but nothing
# answers on it, tell the resolver to prefer ipv4.
set -euo pipefail

probe=https://dl.flathub.org/repo/config
getent hosts dl.flathub.org || true
v4=0
v6=0
curl -4 -fsS --max-time 30 -o /dev/null \
  -w 'ipv4: %{http_code} in %{time_total}s from %{remote_ip}\n' "$probe" && v4=1 || echo "ipv4: no answer"
curl -6 -fsS --max-time 30 -o /dev/null \
  -w 'ipv6: %{http_code} in %{time_total}s from %{remote_ip}\n' "$probe" && v6=1 || echo "ipv6: no answer"

if [ "$v4" = 1 ] && [ "$v6" = 0 ]; then
  echo "ipv6 is broken here; preferring ipv4"
  if [ "$(id -u)" -eq 0 ]; then
    echo 'precedence ::ffff:0:0/96  100' | tee -a /etc/gai.conf
  else
    echo 'precedence ::ffff:0:0/96  100' | sudo tee -a /etc/gai.conf
  fi
fi
