#!/usr/bin/env bash
# Stop the resource sampler and wait for it to flush.
#
# Usage: sampler-stop.sh <state-dir>

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

STATE="${1:?state dir required}"
PIDFILE="$STATE/sampler.pid"
[ -f "$PIDFILE" ] || exit 0

SPID="$(cat "$PIDFILE")"
kill -TERM "$SPID" 2>/dev/null || true

for _ in $(seq 1 50); do
  kill -0 "$SPID" 2>/dev/null || break
  sleep 0.1
done
kill -0 "$SPID" 2>/dev/null && kill -KILL "$SPID" 2>/dev/null || true

rm -f "$PIDFILE"
