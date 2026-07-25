#!/usr/bin/env bash
# Shared helpers sourced by every script in lab/bin.
# Not executable on its own.

set -euo pipefail

LAB_BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$LAB_BIN/../.." && pwd)"
export LAB_BIN REPO_ROOT

OS="$(uname -s)"
export OS

# ---------------------------------------------------------------- logging ----

_c_red=$'\033[31m'; _c_yel=$'\033[33m'; _c_grn=$'\033[32m'; _c_dim=$'\033[2m'; _c_off=$'\033[0m'
[ -t 2 ] || { _c_red=''; _c_yel=''; _c_grn=''; _c_dim=''; _c_off=''; }

info() { printf '%s\n' "$*" >&2; }
step() { printf '%s==>%s %s\n' "$_c_grn" "$_c_off" "$*" >&2; }
warn() { printf '%swarn:%s %s\n' "$_c_yel" "$_c_off" "$*" >&2; }
dim()  { printf '%s%s%s\n' "$_c_dim" "$*" "$_c_off" >&2; }
die()  { printf '%serror:%s %s\n' "$_c_red" "$_c_off" "$*" >&2; exit 1; }

# ----------------------------------------------------------- host profile ----

# load_host [profile]  — source lab/hosts/<profile>.env, default "local".
load_host() {
  local profile="${1:-${HOST:-local}}"
  local f="$REPO_ROOT/lab/hosts/${profile}.env"
  [ -f "$f" ] || die "no host profile at $f"
  # shellcheck disable=SC1090
  set -a; . "$f"; set +a
  : "${HOST_PROFILE:?host profile must define HOST_PROFILE}"
  : "${BASE_URL:?host profile must define BASE_URL}"
  : "${HTTP_PORT:=8080}"
  export HOST_PROFILE BASE_URL HTTP_PORT
}

# load_scenario <scenario-id>  — source scenarios/<id>/scenario.env and export paths.
#
# scenario.env supplies DEFAULTS. Anything the caller already set in the
# environment wins, so `STEADY_DURATION=15s task bench ...` shortens a run without
# editing the scenario. Sourcing alone would silently overwrite the caller.
load_scenario() {
  local id="${1:?scenario id required}"
  SCENARIO_ID="$id"
  SCENARIO_DIR="$REPO_ROOT/scenarios/$id"
  [ -d "$SCENARIO_DIR" ] || die "no scenario at $SCENARIO_DIR"

  local envfile="$SCENARIO_DIR/scenario.env"
  [ -f "$envfile" ] || die "scenario $id has no scenario.env"

  # Remember which of the file's keys the caller had already set.
  local key overridden=""
  for key in $(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$envfile" | tr -d '='); do
    if [ -n "${!key+set}" ] && [ -n "${!key}" ]; then
      overridden="$overridden $key"
      eval "__override_${key}=\${${key}}"
    fi
  done

  # shellcheck disable=SC1090
  set -a; . "$envfile"; set +a

  for key in $overridden; do
    eval "${key}=\${__override_${key}}"
    eval "unset __override_${key}"
    export "${key?}"
  done
  [ -n "$overridden" ] && dim "  scenario overrides:${overridden}"

  : "${ROUTE:?scenario.env must define ROUTE}"
  export SCENARIO_ID SCENARIO_DIR ROUTE
}

# --------------------------------------------------------------- versions ----

octo_version() {
  # "octo 0.4.2" -> "0.4.2"
  command -v octo >/dev/null 2>&1 || { echo "unknown"; return; }
  octo version 2>/dev/null | awk '{print $2; exit}' | tr -d '\r'
}

k6_version() {
  command -v k6 >/dev/null 2>&1 || { echo "unknown"; return; }
  k6 version 2>/dev/null | awk '{print $2; exit}' | tr -d 'v\r'
}

# docker_image_version <image> — the runtime version inside the image.
#
# The published image carries no org.opencontainers.image.version label, so the
# authoritative answer comes from asking the binary. A tag like "latest" is not a
# version and would make results unattributable.
docker_image_version() {
  local image="$1" v

  v="$(docker run --rm --entrypoint /usr/local/bin/octo "$image" version 2>/dev/null \
       | awk '{print $2; exit}' | tr -d '\r')"
  [ -n "$v" ] && { echo "$v"; return; }

  v="$(docker image inspect "$image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null || true)"
  [ -n "$v" ] && [ "$v" != "<no value>" ] && { echo "$v"; return; }

  case "$image" in
    *:*) echo "${image##*:}" ;;
    *)   echo "latest" ;;
  esac
}

# version_under_test <target> — the string that stamps the run id.
version_under_test() {
  case "$1" in
    native) octo_version ;;
    docker) docker_image_version "${OCTO_IMAGE:-juancavallotti/octo-runtime:latest}" ;;
    *) echo "unknown" ;;
  esac
}

# ------------------------------------------------------------------ misc -----

now_utc()   { date -u +%Y-%m-%dT%H:%M:%SZ; }
today()     { date +%Y-%m-%d; }
epoch_ms()  { python3 -c 'import time; print(int(time.time()*1000))'; }

# json_escape <string>
json_escape() { python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' <<<"$1"; }

# wait_ready <url> <timeout-seconds> — poll until 200, echo elapsed ms, non-zero on timeout.
wait_ready() {
  local url="$1" timeout="${2:-30}" start end code
  start="$(epoch_ms)"
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url" 2>/dev/null || echo 000)"
    if [ "$code" = "200" ]; then
      end="$(epoch_ms)"
      echo $(( end - start ))
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# require <cmd> <hint>
require() {
  command -v "$1" >/dev/null 2>&1 || die "missing required tool '$1'. $2"
}
