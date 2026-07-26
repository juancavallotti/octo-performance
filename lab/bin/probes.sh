#!/usr/bin/env bash
# Start a scenario and show what its admin port says. Not a measurement.
#
# Usage: probes.sh
# Environment: SCENARIO (required), TARGET, HOST, BUILD
#
# Exists because "probing by hand" is a documented and legitimate part of forming a
# hypothesis (AGENTS.md), and the two non-negotiables it lists — assert the port is
# free, assert the runtime actually came up — are now questions the runtime can
# answer directly instead of being inferred from a 404.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

: "${SCENARIO:?SCENARIO is required}"
TARGET="${TARGET:-native}"
VARIANT="${VARIANT:-baseline}"

load_host "${HOST:-local}"
load_scenario "$SCENARIO"

DRIVER="$LAB_BIN/target-$TARGET.sh"
[ -x "$DRIVER" ] || die "no driver for target '$TARGET'"

ensure_octo_build "$TARGET"
octo_has_admin_port "$TARGET" \
  || die "this build serves no admin port — measure a source build: BUILD=dev"

export METRICS=1
STAGE="$REPO_ROOT/.stage/probes"
STATE="$REPO_ROOT/.stage/probes-state"
rm -rf "$STATE"; mkdir -p "$STATE"

"$LAB_BIN/stage-config.sh" "$SCENARIO_DIR" "$VARIANT" "$STAGE" >/dev/null

cleanup() { "$DRIVER" stop "$STATE" 2>/dev/null || true; scenario_teardown; }
scenario_setup
trap cleanup EXIT

step "starting $SCENARIO_ID ($VARIANT) on $TARGET"
"$DRIVER" start "$STAGE" "$STATE" >/dev/null
if ! ready_ms="$(readiness_probe 60)"; then
  cat "$STATE/octo.log" >&2 2>/dev/null || true
  die "target never became ready"
fi
dim "  ready in ${ready_ms}ms"

info ""
step "admin port $(admin_url)"
info ""
for path in / /healthz /readyz; do
  printf '  %-10s %s\n' "$path" "$(curl -s --max-time 2 -w ' [%{http_code}]' "$(admin_url "$path")" \
    2>/dev/null | tr '\n' ' ')" >&2
done

info ""
step "identity"
curl -s --max-time 5 "$(admin_url /metrics)" | python3 "$LAB_BIN/metrics-identity.py" >&2

info ""
step "a request through the flow, then the counters it moved"
curl -s -o /dev/null -w '  %s -> %%{http_code}\n' "${BASE_URL}${ROUTE}" >&2 || true
info ""
curl -s --max-time 5 "$(admin_url /metrics)" \
  | grep -E '^octo_(flow_messages_total|flow_in_flight|ready|flows|connectors)' >&2 || true

info ""
info "  admin port:  $(admin_url)"
info "  scenario:    ${BASE_URL}${ROUTE}"
info ""
read -r -p "  press enter to stop " _ </dev/tty || true
