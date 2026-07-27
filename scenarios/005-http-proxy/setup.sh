#!/usr/bin/env bash
# Start the backend the proxy talks to.
#
# Runs outside the measured window. The backend is a separate process so that its cost
# never lands in the runtime's CPU sample — the harness samples only the octo process.
#
# It binds 0.0.0.0 rather than loopback, because on a split topology the runtime is on
# another machine. The firewall is what keeps that from meaning "the internet", and the
# deps host has no external address in any case.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
PORT="${BACKEND_PORT:-9090}"
STAGE="$ROOT/.stage"
PIDFILE="$STAGE/backend.pid"
LOG="$STAGE/backend.log"

mkdir -p "$STAGE/bin"

# Prefer the shipped binary. It comes out of the same release archive as the harness and
# the scenarios, so its version is the campaign's version — and this process's response
# size and delay are what scenario 005 actually measures, which makes "which build of
# the backend" a question a result has to be able to answer.
#
# Building from source is the fallback for a working checkout, where there is no
# release. It is a fallback and not the default: a dependency compiled at setup time is
# a dependency whose version nobody recorded.
BACKEND_BIN=""
for candidate in "$ROOT/labbackend" "$ROOT/bin/labbackend" "$STAGE/bin/labbackend"; do
  if [ -x "$candidate" ]; then
    BACKEND_BIN="$candidate"
    break
  fi
done

if [ -z "$BACKEND_BIN" ]; then
  SRC="$ROOT/harness/cmd/labbackend"
  [ -d "$SRC" ] || {
    echo "error: no labbackend binary and no source at $SRC" >&2
    echo "       a release archive ships one; a checkout needs the Go toolchain" >&2
    exit 1
  }
  command -v go >/dev/null 2>&1 || {
    echo "error: scenario 005 needs either a shipped labbackend or the Go toolchain" >&2
    exit 1
  }
  BACKEND_BIN="$STAGE/bin/labbackend"
  # Rebuild only when the source is newer, so repeated runs do not pay for it.
  if [ ! -x "$BACKEND_BIN" ] || [ "$SRC/main.go" -nt "$BACKEND_BIN" ]; then
    echo "  building labbackend from source" >&2
    (cd "$ROOT/harness" && go build -o "$BACKEND_BIN" ./cmd/labbackend)
  fi
fi

# Clear a leftover from an interrupted run.
if [ -f "$PIDFILE" ]; then
  kill "$(cat "$PIDFILE")" 2>/dev/null || true
  rm -f "$PIDFILE"
fi

"$BACKEND_BIN" -addr ":$PORT" \
  -size "${BACKEND_SIZE:-1024}" \
  -delay "${BACKEND_DELAY:-70ms}" \
  >"$LOG" 2>&1 &
echo $! > "$PIDFILE"

# Wait for it before handing back, or the first cell races the backend's bind.
for _ in $(seq 1 50); do
  if curl -sf -o /dev/null --max-time 1 "http://localhost:$PORT/health" 2>/dev/null; then
    echo "  backend ready on :$PORT (delay=${BACKEND_DELAY:-70ms} size=${BACKEND_SIZE:-1024}) from $BACKEND_BIN" >&2
    exit 0
  fi
  sleep 0.1
done

echo "error: backend never became ready on :$PORT" >&2
cat "$LOG" >&2 || true
exit 1
