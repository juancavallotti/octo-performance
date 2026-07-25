#!/usr/bin/env bash
# Target driver: the published octo runtime container image.
#
# Usage:
#   target-docker.sh start <config-dir> <state-dir>
#   target-docker.sh pid   <state-dir>     # container id to sample
#   target-docker.sh stop  <state-dir>
#
# Cumulative CPU comes from the Docker Engine API (cpu_stats.cpu_usage.total_usage,
# nanoseconds) rather than from `docker stats` percentages, so the container target
# is measured with the same rigour as the native one.
#
# CPU_LIMIT sizes the container the way a commercial platform sizes a a hosted platform worker. Their report
# defines a CPU as "the number of CPU cores available to a given deployment" and
# publishes every number at 0.1, 1, and 4 CPUs; `--cpus` is the same quantity, so
# running at CPU_LIMIT=1 puts our numbers on their x-axis instead of on our laptop's.
# See COMPARISON.md.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

cmd="${1:?usage: target-docker.sh start|pid|stop ...}"; shift

CONTAINER_NAME="${CONTAINER_NAME:-octo-bench}"

start() {
  local config_dir="${1:?config dir required}" state="${2:?state dir required}"
  require docker "Install Docker Desktop."
  docker info >/dev/null 2>&1 || die "docker daemon not reachable"
  mkdir -p "$state"

  local image="${OCTO_IMAGE:-juancavallotti/octo-runtime:latest}"
  docker image inspect "$image" >/dev/null 2>&1 || die "image $image not pulled — run: docker pull $image"

  # Remove any leftover from an interrupted run.
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

  # The tuning knobs are baked into the rendered config rather than passed here —
  # Octo's ${ENV} substitution does not reach root-flow fields. Only LOG_LEVEL is
  # forwarded, so the container logs at the same verbosity as the native run.
  local env_args=()
  [ -n "${LOG_LEVEL:-}" ] && env_args+=(-e "LOG_LEVEL=$LOG_LEVEL")

  # A scenario's dependencies (a database, a backend server) run on the host, and
  # "localhost" inside a container is the container. DOCKER_ENV lets a scenario
  # restate those addresses for this target — it is a property of the target, not
  # of the integration, which is why it lives in scenario.env rather than in the
  # YAML. Entries are NAME=VALUE, space separated.
  local kv
  for kv in ${DOCKER_ENV:-}; do
    env_args+=(-e "$kv")
  done

  # Deployment envelope. Unset means "whatever the host has", which is the right
  # default for tracking Octo against itself; CPU_LIMIT is for the cross-vendor
  # comparison, where the envelope has to match the one the other vendor published.
  local limit_args=()
  if [ -n "${CPU_LIMIT:-}" ]; then
    limit_args+=(--cpus "$CPU_LIMIT")
    local ncpu
    ncpu="$(docker info --format '{{.NCPU}}' 2>/dev/null || echo 0)"
    if [ "$ncpu" != "0" ] && awk "BEGIN{exit !($CPU_LIMIT > $ncpu)}"; then
      warn "CPU_LIMIT=$CPU_LIMIT exceeds the ${ncpu} CPUs the daemon has; the limit will not bind"
    fi
    # Sized from a commercial platform's own instance table so the memory envelope matches the
    # CPU one: 0.1 CPU was a t3.micro (1 GB), 1 CPU a t3.medium (4 GB), and the
    # 4 CPU on-premise case a c5n.xlarge (10.5 GB).
    if [ -z "${MEM_LIMIT:-}" ]; then
      case "$CPU_LIMIT" in
        0.1) MEM_LIMIT=1g ;;
        1)   MEM_LIMIT=4g ;;
        4)   MEM_LIMIT=10g ;;
      esac
    fi
    [ -n "${MEM_LIMIT:-}" ] && limit_args+=(--memory "$MEM_LIMIT")
  fi

  # The runtime binds 8080 inside the container; publish it on the host port the
  # host profile declares.
  local cid
  cid="$(docker run -d \
    --name "$CONTAINER_NAME" \
    -p "${HTTP_PORT:-8080}:8080" \
    -e HTTP_PORT=8080 \
    ${env_args[@]+"${env_args[@]}"} \
    ${limit_args[@]+"${limit_args[@]}"} \
    -v "$config_dir:/etc/octo/integrations:ro" \
    "$image")" || die "docker run failed"

  echo "$cid" > "$state/pid"
  echo "$image" > "$state/image"
  # What the container was actually given, read back from the daemon rather than
  # from what we asked for — a limit that silently failed to apply would otherwise
  # be published as though it held.
  docker inspect "$cid" --format \
    '{"nanoCpus":{{.HostConfig.NanoCpus}},"cpuQuota":{{.HostConfig.CpuQuota}},"cpuPeriod":{{.HostConfig.CpuPeriod}},"memoryBytes":{{.HostConfig.Memory}}}' \
    > "$state/limits.json" 2>/dev/null || true
  printf '%s\n' "$cid"
}

pid() {
  local state="${1:?state dir required}"
  [ -f "$state/pid" ] || die "no container id recorded in $state"
  cat "$state/pid"
}

stop() {
  local state="${1:?state dir required}"
  local cid
  cid="$(cat "$state/pid" 2>/dev/null || true)"
  [ -n "$cid" ] || return 0

  # Capture the container's own logs before it disappears.
  docker logs "$cid" >"$state/octo.log" 2>&1 || true

  # Final cumulative counters stand in for /usr/bin/time on the native target.
  "$LAB_BIN/docker-stats.sh" "$cid" > "$state/final-stats.json" 2>/dev/null || true

  docker stop -t 10 "$cid" >/dev/null 2>&1 || true
  docker rm -f "$cid" >/dev/null 2>&1 || true
  rm -f "$state/pid"
}

case "$cmd" in
  start) start "$@" ;;
  pid)   pid   "$@" ;;
  stop)  stop  "$@" ;;
  *) die "unknown command '$cmd'" ;;
esac
