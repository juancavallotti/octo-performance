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
  # Readiness is probed with a bare GET. A scenario whose ROUTE needs a request
  # body (or a method other than GET) declares READY_ROUTE pointing at a trivial
  # health flow instead — the runtime has no built-in health endpoint, so each
  # scenario that needs one provides it.
  : "${READY_ROUTE:=$ROUTE}"
  export SCENARIO_ID SCENARIO_DIR ROUTE READY_ROUTE
}

# The URL the harness polls to decide the target is up.
ready_url() { printf '%s%s\n' "$BASE_URL" "${READY_ROUTE:-$ROUTE}"; }

# Scenario dependency lifecycle. A scenario that needs external infrastructure —
# a database, a broker — provides setup.sh and teardown.sh next to its
# scenario.env. Both are optional and run outside the measured window, so
# container start-up and schema creation never land inside a benchmark.
scenario_setup() {
  [ -x "$SCENARIO_DIR/setup.sh" ] || return 0
  step "scenario setup"
  "$SCENARIO_DIR/setup.sh" || die "scenario setup failed"
}

scenario_teardown() {
  [ -x "$SCENARIO_DIR/teardown.sh" ] || return 0
  step "scenario teardown"
  "$SCENARIO_DIR/teardown.sh" || warn "scenario teardown reported an error"
}

# --------------------------------------------------------------- versions ----

# ------------------------------------------------------------ build channel ---
#
# BUILD is a second axis, orthogonal to TARGET. TARGET says how the runtime is
# deployed (a binary on the host, or the container image). BUILD says where the
# artifact came from:
#
#   release   the published distribution — what users actually run
#   dev       built here from a source checkout — unreleased work
#
# Both combinations are meaningful, and they are kept apart everywhere: a dev
# build's version string carries its source commit, so it gets its own run id and
# its own row in the regression view rather than being filed as a repeat of the
# release it was branched from.
# Validated once, here, rather than inside build_channel: that function is called
# from inside $( ), where `die` would only kill the subshell and a typo would sail
# through as if it had said "release".
case "${BUILD:-release}" in
  release|dev) ;;
  *) die "unknown BUILD '${BUILD}' (expected release or dev)" ;;
esac

build_channel() { printf '%s\n' "${BUILD:-release}"; }

# ensure_octo_build <target> — build the dev artifact once, at the entry point.
#
# Exported so child scripts within the same run resolve the artifact without each
# one shelling out to the builder. A no-op for BUILD=release.
ensure_octo_build() {
  [ "$(build_channel)" = "dev" ] || return 0
  local t="${1:-${TARGET:-native}}"
  local ref
  ref="$("$LAB_BIN/build-octo.sh" "$t")" || die "dev build failed"
  export OCTO_DEV_ARTIFACT="$ref"
}

# The dev artifact for a target.
#
# Resolution comes first and a build only happens if nothing is there yet, so the
# artifact is frozen for the whole run: a source tree edited between repetitions
# cannot silently swap the binary halfway through a measurement.
dev_artifact() {
  [ -n "${OCTO_DEV_ARTIFACT:-}" ] && { printf '%s\n' "$OCTO_DEV_ARTIFACT"; return 0; }
  local t="${1:-${TARGET:-native}}" ref
  if ref="$("$LAB_BIN/build-octo.sh" "$t" --resolve 2>/dev/null)"; then
    printf '%s\n' "$ref"
    return 0
  fi
  "$LAB_BIN/build-octo.sh" "$t"
}

# One field out of the dev build's provenance record.
dev_build_field() {
  local key="$1" t="${2:-${TARGET:-native}}"
  "$LAB_BIN/build-octo.sh" "$t" --meta 2>/dev/null \
    | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
for part in sys.argv[1].split("."):
    d = (d or {}).get(part) if isinstance(d, dict) else None
# Booleans come back as shell-comparable "true"/"false", not Python "True".
print("" if d is None else ("true" if d is True else "false" if d is False else d))' "$key"
}

# The native binary under test. Defaults to whatever is on PATH; set OCTO_BIN to
# benchmark a specific build — e.g. two releases side by side on the same host,
# which is the whole point of tracking regressions. BUILD=dev overrides both with
# the artifact built from source.
octo_bin() {
  if [ "$(build_channel)" = "dev" ]; then
    dev_artifact native
  elif [ -n "${OCTO_BIN:-}" ]; then
    printf '%s\n' "$OCTO_BIN"
  else
    command -v octo 2>/dev/null || true
  fi
}

# The container image under test, honouring the same axis.
octo_image() {
  if [ "$(build_channel)" = "dev" ]; then
    dev_artifact docker
  else
    printf '%s\n' "${OCTO_IMAGE:-juancavallotti/octo-runtime:latest}"
  fi
}

octo_version() {
  # "octo 0.4.2" -> "0.4.2"
  local bin; bin="$(octo_bin)"
  [ -n "$bin" ] && [ -x "$bin" ] || { echo "unknown"; return; }
  "$bin" version 2>/dev/null | awk '{print $2; exit}' | tr -d '\r'
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
#
# For a dev build this is the buildId, e.g. "0.4.3-dev.dd065f8": the source
# declares the same version constant as the last release, so without the commit
# a dev run and the release run collide on the run id and the regression view
# reads them as one version measured twice.
version_under_test() {
  if [ "$(build_channel)" = "dev" ]; then
    local id; id="$(dev_build_field buildId "$1")"
    [ -n "$id" ] && { printf '%s\n' "$id"; return; }
    echo "unknown-dev"
    return
  fi
  case "$1" in
    native) octo_version ;;
    docker) docker_image_version "$(octo_image)" ;;
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
