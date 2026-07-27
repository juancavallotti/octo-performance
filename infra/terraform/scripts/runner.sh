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
apt-get install -y k6

# The Go toolchain: the harness is built from the checked-out repository rather than
# downloaded, because the version of the *measuring instrument* has to be pinned to the
# same commit as the scenarios it runs. A harness release and a scenario tree that
# disagree is a class of failure with no symptom.
GO_VERSION=1.26.4
ARCH=$(dpkg --print-architecture)
curl -fsSL "https://go.dev/dl/go$${GO_VERSION}.linux-$${ARCH}.tar.gz" -o /tmp/go.tgz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tgz
ln -sf /usr/local/go/bin/go /usr/local/bin/go
rm -f /tmp/go.tgz

install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf /srv/perf/campaigns

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
