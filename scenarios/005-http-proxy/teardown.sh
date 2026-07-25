#!/usr/bin/env bash
# Stop the proxy backend. Safe to run when it was never started.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PIDFILE="$REPO_ROOT/.stage/backend.pid"

[ -f "$PIDFILE" ] || exit 0
kill "$(cat "$PIDFILE")" 2>/dev/null || true
rm -f "$PIDFILE"
echo "  backend stopped"
