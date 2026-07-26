#!/usr/bin/env bash
# Stop the observers and wait for them to flush.
#
# Usage: sampler-stop.sh <state-dir>
#
# Called before the target is stopped, never after: the metrics scraper's last
# snapshot is the end of the measured window, and a runtime that has already been
# sent SIGTERM is draining — its in-flight count is falling and its readiness has
# already flipped, neither of which belongs in the window being reported.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

STATE="${1:?state dir required}"

# Signal both observers before waiting on either, so the wait is concurrent.
pids=""
for name in sampler scraper; do
  f="$STATE/$name.pid"
  [ -f "$f" ] || continue
  p="$(cat "$f")"
  kill -TERM "$p" 2>/dev/null || true
  pids="$pids $p"
  rm -f "$f"
done

for p in $pids; do
  for _ in $(seq 1 50); do
    kill -0 "$p" 2>/dev/null || break
    sleep 0.1
  done
  kill -0 "$p" 2>/dev/null && kill -KILL "$p" 2>/dev/null || true
done
