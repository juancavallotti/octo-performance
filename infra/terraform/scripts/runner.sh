#!/usr/bin/env bash
# The runner: the harness, the load generator, and nothing else.
#
# Nothing else is the point. Every process on this machine competes with k6 for the
# cores that decide whether the generator can place the load it was asked to, and the
# whole reason this VM exists is that the old lab could not make that claim.
set -euo pipefail

exec > >(tee -a /var/log/perf-startup.log) 2>&1
echo "=== runner startup $(date -Is) ==="

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y --no-install-recommends ca-certificates curl gnupg git jq

# k6 from Grafana's repository rather than a tarball, so the version is upgradable in
# place and `k6 version` reports something the report can record as provenance.
mkdir -p /etc/apt/keyrings
curl -fsSL https://dl.k6.io/key.gpg | gpg --dearmor -o /etc/apt/keyrings/k6.gpg
echo "deb [signed-by=/etc/apt/keyrings/k6.gpg] https://dl.k6.io/deb stable main" \
  > /etc/apt/sources.list.d/k6.list
apt-get update -y
# Pinned, and the pin is the point. `apt-get install -y k6` takes whatever is newest in
# the repository, which is how a runner declared as k6_version = "1.3.0" came up running
# v2.0.0 — a major version across the summary format the harness parses, recorded in
# every result's fingerprint as the version it was not. The variable was passed to this
# template and never read, so nothing disagreed out loud.
#
# An absent version fails the boot here rather than silently installing another one.
apt-get install -y "k6=${k6_version}"
# The campaign outlives the install. An unattended upgrade that bumps k6 at cell forty
# changes the instrument mid-measurement, and the interleaved ordering cannot balance out
# something that happens once.
apt-mark hold k6

install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf /srv/perf/campaigns

# The lab: perf, labbackend, every scenario and every campaign spec, in one verified
# archive. Nothing is built here and no repository is cloned — a runner that fetched a
# binary from a release and scenarios from git could have the two disagree, silently,
# because a scenario that loads is not the same as a scenario the harness was written
# against.
LAB_VERSION="${lab_version}"
LAB_REPO="${lab_repo}"
LAB_ROOT=/srv/perf
LAB_USER="${ssh_user}"
# shellcheck source=/dev/null
. /tmp/install-lab.sh
install_lab

# The tuning that keeps the generator from being the bottleneck for a reason that has
# nothing to do with cores.
#
# A load generator at ten thousand requests a second opens and closes ten thousand
# sockets a second. The defaults run out of ephemeral ports and then out of file
# descriptors, and both failures arrive as connection errors that read in a summary as
# the server refusing load.
cat > /etc/sysctl.d/99-perf-runner.conf <<'SYSCTL'
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

echo "=== runner ready $(date -Is) ==="
