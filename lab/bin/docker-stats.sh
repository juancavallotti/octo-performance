#!/usr/bin/env bash
# Read one cumulative stats snapshot for a container from the Docker Engine API.
#
# Usage: docker-stats.sh <container-id>
# Emits: {"cpuNanos": <cumulative user+sys CPU in ns>, "rssBytes": <current>, "online_cpus": n}
#
# `docker stats` only exposes an instantaneous CPU percentage. The Engine API
# exposes cpu_stats.cpu_usage.total_usage, which is cumulative — differentiating it
# between samples gives exact CPU-seconds per interval, matching how the native
# target is measured.

set -euo pipefail

CID="${1:?usage: docker-stats.sh <container-id>}"

SOCK="${DOCKER_SOCK:-}"
if [ -z "$SOCK" ]; then
  SOCK="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null | sed 's|^unix://||' || true)"
fi
[ -n "$SOCK" ] || SOCK=/var/run/docker.sock

curl -s --unix-socket "$SOCK" "http://localhost/containers/${CID}/stats?stream=false" 2>/dev/null \
| python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("{}"); raise SystemExit(0)

cpu = d.get("cpu_stats", {}) or {}
mem = d.get("memory_stats", {}) or {}
usage = (cpu.get("cpu_usage") or {}).get("total_usage", 0)

# Docker reports memory_stats.usage including page cache; subtract the cache
# component so the number is comparable to RSS on the native target.
rss = mem.get("usage", 0)
stats = mem.get("stats") or {}
for cache_key in ("inactive_file", "total_inactive_file", "cache"):
    if cache_key in stats:
        rss = max(0, rss - stats[cache_key])
        break

print(json.dumps({
    "cpuNanos": usage,
    "rssBytes": rss,
    "onlineCpus": cpu.get("online_cpus", 0),
}))
'
