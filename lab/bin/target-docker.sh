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

  # The runtime binds 8080 inside the container; publish it on the host port the
  # host profile declares.
  local cid
  cid="$(docker run -d \
    --name "$CONTAINER_NAME" \
    -p "${HTTP_PORT:-8080}:8080" \
    -e HTTP_PORT=8080 \
    ${env_args[@]+"${env_args[@]}"} \
    -v "$config_dir:/etc/octo/integrations:ro" \
    "$image")" || die "docker run failed"

  echo "$cid" > "$state/pid"
  echo "$image" > "$state/image"
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
