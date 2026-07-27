#!/usr/bin/env bash
# The subject: the runtime under test, and nothing else.
#
# No harness, no agent, no load generator. The harness drives this machine over SSH and
# samples its /proc through the same connection, so there is no second binary here whose
# version could disagree with the runner's — which is a failure mode with no symptom
# except numbers that are quietly wrong.
set -euo pipefail

exec > >(tee -a /var/log/perf-startup.log) 2>&1
echo "=== subject startup $(date -Is) ==="

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y --no-install-recommends ca-certificates curl

# Staging root. The harness is given this as --subject-dir and puts each cell's rendered
# config and integration tree underneath it.
install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf
install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf/octo-versions

# The releases under test, placed at boot rather than pushed per cell: pushing a binary
# is a difference between the first cell of a campaign and every other one.
#
# When no URL template is configured the operator stages them by hand, which is the
# honest arrangement for a runtime with no public release URL. A template that 404s at
# boot would fail the campaign at its first cell instead of here.
VERSIONS="${versions}"
URL_TEMPLATE="${url_template}"
if [ -n "$VERSIONS" ] && [ -n "$URL_TEMPLATE" ]; then
  for v in $VERSIONS; do
    url=$(printf "$URL_TEMPLATE" "$v")
    dest="/srv/perf/octo-versions/octo-$v"
    echo "fetching octo $v from $url"
    # Releases ship a tarball, not a bare binary, so the archive is unpacked and the
    # one member that matters is installed. Curling a .tar.gz straight to the
    # destination path produces a file that is executable, correctly named, and gzip —
    # and the campaign discovers that at its first cell, as a runtime that will not
    # start, rather than here.
    tmp=$(mktemp -d)
    if case "$url" in
         *.tar.gz|*.tgz) curl -fsSL "$url" | tar xz -C "$tmp" octo && mv "$tmp/octo" "$dest" ;;
         *)              curl -fsSL "$url" -o "$dest" ;;
       esac
    then
      chmod 0755 "$dest"
      chown "${ssh_user}:${ssh_user}" "$dest"
      echo "staged $dest"
    else
      # Loud, and not fatal. A campaign naming a version that is not here fails at
      # its first cell with a message that says which one; a boot that dies here
      # leaves a machine that never finishes starting and says nothing about why.
      echo "WARNING: could not fetch octo $v from $url" >&2
      rm -f "$dest"
    fi
    rm -rf "$tmp"
  done
fi

# The same socket and descriptor limits as the runner. A server that runs out of file
# descriptors under load returns errors that look exactly like a server that is slow,
# and the difference decides whether a result is a regression or an artifact.
cat > /etc/sysctl.d/99-perf-subject.conf <<'SYSCTL'
net.ipv4.ip_local_port_range = 10000 65535
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_tw_reuse = 1
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
fs.file-max = 2097152
SYSCTL
sysctl --system

cat > /etc/security/limits.d/99-perf.conf <<'LIMITS'
*  soft  nofile  1048576
*  hard  nofile  1048576
LIMITS

# Unattended upgrades are disabled for the life of the campaign.
#
# A package upgrade partway through is a change to the machine under measurement, and
# the interleaved ordering exists precisely to keep arms from differing by anything
# except the arm. apt deciding to run in the middle of cell forty is a confound the
# rotation cannot balance out.
systemctl disable --now unattended-upgrades 2>/dev/null || true
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true

echo "=== subject ready $(date -Is) ==="
