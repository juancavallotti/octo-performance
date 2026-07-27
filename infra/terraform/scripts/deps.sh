#!/usr/bin/env bash
# The dependency host: Postgres for scenario 003, the slow backend for scenario 005.
#
# Its own machine, because the old lab reached both through host.docker.internal — the
# database shared the subject's cores, and the measurement folded its CPU into the
# runtime's. METHODOLOGY.md calls the numbers that produced "not merely indicative — it
# is misleading", and this VM is the whole remedy.
set -euo pipefail

exec > >(tee -a /var/log/perf-startup.log) 2>&1
echo "=== deps startup $(date -Is) ==="

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y --no-install-recommends ca-certificates curl gnupg git

# Docker, for scenario 003's Postgres. The scenario's own setup.sh brings the container
# up and creates the schema; this only puts the daemon there.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io
usermod -aG docker "${ssh_user}" || true

# The Go toolchain, because scenario 005's setup.sh builds lab/backend from source.
GO_VERSION=1.26.4
ARCH=$(dpkg --print-architecture)
curl -fsSL "https://go.dev/dl/go$${GO_VERSION}.linux-$${ARCH}.tar.gz" -o /tmp/go.tgz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tgz
ln -sf /usr/local/go/bin/go /usr/local/bin/go
rm -f /tmp/go.tgz

install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf

# Postgres and the backend must answer on the subnet address, not only on loopback.
# Scenario setup scripts bind to 0.0.0.0 already; the firewall is what keeps that from
# meaning "the internet", and this host has no external address in any case.
cat > /etc/sysctl.d/99-perf-deps.conf <<'SYSCTL'
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
SYSCTL
sysctl --system

systemctl disable --now unattended-upgrades 2>/dev/null || true

echo "=== deps ready $(date -Is) ==="
