#!/usr/bin/env bash
# Target driver: the standalone octo distribution running directly on the host.
#
# Usage:
#   target-native.sh start <config-dir> <state-dir>
#   target-native.sh pid   <state-dir>     # pid to sample
#   target-native.sh stop  <state-dir>
#
# The process is wrapped in /usr/bin/time so that whole-lifetime user+sys CPU and
# maximum RSS are recorded authoritatively rather than inferred from 1 Hz samples.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

cmd="${1:?usage: target-native.sh start|pid|stop ...}"; shift

start() {
  local config_dir="${1:?config dir required}" state="${2:?state dir required}"
  local bin; bin="$(octo_bin)"
  [ -n "$bin" ] && [ -x "$bin" ] || \
    die "no octo binary (set OCTO_BIN, or install: https://juancavallotti.github.io/octo/getting-started/installation/)"
  mkdir -p "$state"

  local time_args
  if [ "$OS" = "Darwin" ]; then time_args=(-l -o "$state/time.txt")
  else                          time_args=(-v -o "$state/time.txt"); fi

  # The tuning knobs are baked into the rendered config, not passed here: Octo's
  # ${ENV} substitution does not reach root-flow fields.
  /usr/bin/time "${time_args[@]}" \
    "$bin" run --config "$config_dir" >"$state/octo.log" 2>&1 &
  local wrapper=$!
  echo "$wrapper" > "$state/wrapper.pid"

  # /usr/bin/time forks the real process; sample the child, not the wrapper.
  local child="" i
  for i in $(seq 1 100); do
    child="$(pgrep -P "$wrapper" 2>/dev/null | head -1 || true)"
    [ -n "$child" ] && break
    kill -0 "$wrapper" 2>/dev/null || die "octo exited immediately; see $state/octo.log"
    sleep 0.05
  done
  [ -n "$child" ] || die "could not find the octo process under /usr/bin/time"
  echo "$child" > "$state/pid"
  printf '%s\n' "$child"
}

pid() {
  local state="${1:?state dir required}"
  [ -f "$state/pid" ] || die "no pid recorded in $state"
  cat "$state/pid"
}

stop() {
  local state="${1:?state dir required}"
  local child wrapper i
  child="$(cat "$state/pid" 2>/dev/null || true)"
  wrapper="$(cat "$state/wrapper.pid" 2>/dev/null || true)"

  [ -n "$child" ] && kill -TERM "$child" 2>/dev/null || true

  # Wait for the wrapper so time.txt is flushed before anyone reads it.
  if [ -n "$wrapper" ]; then
    for i in $(seq 1 100); do
      kill -0 "$wrapper" 2>/dev/null || break
      sleep 0.1
    done
    kill -0 "$wrapper" 2>/dev/null && kill -KILL "$child" "$wrapper" 2>/dev/null || true
  fi
  rm -f "$state/pid" "$state/wrapper.pid"
}

case "$cmd" in
  start) start "$@" ;;
  pid)   pid   "$@" ;;
  stop)  stop  "$@" ;;
  *) die "unknown command '$cmd'" ;;
esac
