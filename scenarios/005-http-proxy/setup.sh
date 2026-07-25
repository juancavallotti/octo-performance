#!/usr/bin/env bash
# Build and start the backend the proxy talks to.
#
# Runs outside the measured window. The backend is a separate process so that its
# cost never lands in the runtime's CPU sample — the harness samples only the Octo
# process or container.
#
# Caveat worth stating: a commercial platform put their backend on a separate EC2 instance. Ours
# shares the host with both the runtime and the load generator. It is a sleeping
# server rather than a working one precisely to keep that contention small, but it
# is not zero. See COMPARISON.md.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
BACKEND_SRC="$REPO_ROOT/lab/backend"
BACKEND_BIN="$REPO_ROOT/.stage/bin/bench-backend"
PORT="${BACKEND_PORT:-9090}"
PIDFILE="$REPO_ROOT/.stage/backend.pid"

command -v go >/dev/null 2>&1 || {
  echo "error: scenario 005 needs the Go toolchain to build lab/backend" >&2
  exit 1
}

mkdir -p "$(dirname "$BACKEND_BIN")"

# Rebuild only when the source is newer, so repeated runs do not pay for it.
if [ ! -x "$BACKEND_BIN" ] || [ "$BACKEND_SRC/main.go" -nt "$BACKEND_BIN" ]; then
  echo "  building lab/backend"
  (cd "$BACKEND_SRC" && go build -o "$BACKEND_BIN" .)
fi

# Clear a leftover from an interrupted run.
if [ -f "$PIDFILE" ]; then
  kill "$(cat "$PIDFILE")" 2>/dev/null || true
  rm -f "$PIDFILE"
fi

"$BACKEND_BIN" -addr ":$PORT" \
  -size "${BACKEND_SIZE:-1024}" \
  -delay "${BACKEND_DELAY:-70ms}" \
  >"$REPO_ROOT/.stage/backend.log" 2>&1 &
echo $! > "$PIDFILE"

# Wait for it before handing back, or the smoke gate races the backend's bind.
for _ in $(seq 1 50); do
  if curl -sf -o /dev/null --max-time 1 "http://localhost:$PORT/health" 2>/dev/null; then
    echo "  backend ready on :$PORT (delay=${BACKEND_DELAY:-70ms} size=${BACKEND_SIZE:-1024})"
    exit 0
  fi
  sleep 0.1
done

echo "error: backend never became ready on :$PORT" >&2
cat "$REPO_ROOT/.stage/backend.log" >&2 || true
exit 1
