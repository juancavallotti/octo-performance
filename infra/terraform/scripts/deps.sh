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

install -d -o "${ssh_user}" -g "${ssh_user}" /srv/perf

# The same archive as the runner, unpacked to the same path.
#
# That is a requirement, not tidiness. A scenario's setup.sh is addressed by the path
# the harness knows it by, and the harness runs on the runner — so this host is asked to
# execute /srv/perf/scenarios/005-http-proxy/setup.sh and must actually have it there.
# It also brings labbackend, so scenario 005's dependency is a shipped artifact with a
# recorded version rather than something compiled at setup time.
LAB_VERSION="${lab_version}"
LAB_REPO="${lab_repo}"
LAB_ROOT=/srv/perf
LAB_USER="${ssh_user}"
# shellcheck source=/dev/null
. /tmp/install-lab.sh
install_lab

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
