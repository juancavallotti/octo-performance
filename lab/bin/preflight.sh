#!/usr/bin/env bash
# Verify the lab has what it needs and print the versions that will be recorded.
# Exits non-zero if a hard requirement is missing.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

TARGET="${TARGET:-native}"
load_host "${HOST:-local}"

missing=0
ok()      { printf '  %s✓%s %-12s %s\n' "$_c_grn" "$_c_off" "$1" "$2" >&2; }
absent()  { printf '  %s✗%s %-12s %s\n' "$_c_red" "$_c_off" "$1" "$2" >&2; missing=$((missing+1)); }
skipped() { printf '  %s·%s %-12s %s\n' "$_c_dim" "$_c_off" "$1" "$2" >&2; }

step "preflight (host=$HOST_PROFILE target=$TARGET build=$(build_channel))"

# A dev build is produced here, before anything is checked, so the rest of this
# script reports on the artifact that will actually be measured rather than on
# whatever happened to be lying around from a previous commit.
ensure_octo_build "$TARGET"

# --- hard requirements, every target -----------------------------------------
for t in curl python3 awk; do
  if command -v "$t" >/dev/null 2>&1; then ok "$t" "$(command -v "$t")"; else absent "$t" "not on PATH"; fi
done

if command -v k6 >/dev/null 2>&1; then
  ok "k6" "$(k6_version)"
else
  absent "k6" "not installed — run: brew install k6"
fi

# --- target-specific ----------------------------------------------------------
case "$TARGET" in
  native)
    octo_path="$(octo_bin)"
    if [ -n "$octo_path" ] && [ -x "$octo_path" ]; then
      # version_under_test, not octo_version: a dev binary reports the same
      # version constant as the release it branched from, and printing that bare
      # string here is exactly the confusion this axis exists to prevent.
      ok "octo" "$(version_under_test native)  ($octo_path)"
    else
      absent "octo" "not found${OCTO_BIN:+ at OCTO_BIN=$OCTO_BIN} — see https://juancavallotti.github.io/octo/getting-started/installation/"
    fi
    skipped "docker" "not needed for TARGET=native"
    ;;
  docker)
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
      ok "docker" "$(docker info --format '{{.ServerVersion}} cpus={{.NCPU}} mem={{.MemTotal}}' 2>/dev/null)"
      image="$(octo_image)"
      if [ -n "$image" ] && docker image inspect "$image" >/dev/null 2>&1; then
        ok "image" "$image ($(docker_image_version "$image"))"
      elif [ "$(build_channel)" = "dev" ]; then
        absent "image" "${image:-dev image} not built — run: task build TARGET=docker"
      else
        absent "image" "$image not pulled — run: docker pull $image"
      fi
    else
      absent "docker" "daemon not reachable"
    fi
    skipped "octo" "not needed for TARGET=docker"
    ;;
  *) die "unknown TARGET '$TARGET' (expected native or docker)" ;;
esac

# --- what the scenario needs from the runtime ---------------------------------
# A scenario may declare REQUIRES_CEL: an expression the runtime under test has to
# be able to evaluate. Asked here rather than discovered at flow-build time, so
# "this build is too old for this scenario" is one line at the top instead of a
# compile error buried in octo.log once the dependencies are already up.
#
# Skipped when something above is already missing — there is no artifact to ask.
if [ -n "${SCENARIO:-}" ] && [ "$missing" -eq 0 ]; then
  probe="$(scenario_cel_probe "$SCENARIO")"
  if [ -n "$probe" ]; then
    reason="$(cel_missing "$probe" "$TARGET")"
    if [ -z "$reason" ]; then
      ok "cel" "$SCENARIO's expressions compile against this build"
    else
      absent "cel" "$SCENARIO needs CEL support this build lacks — $reason"
      dim "    the probe is REQUIRES_CEL in scenarios/$SCENARIO/scenario.env"
      [ "$(build_channel)" = "release" ] && \
        dim "    if the support is unreleased, measure the source build: BUILD=dev"
    fi
  fi
fi

# --- what the runtime can tell us about itself --------------------------------
# Not a requirement: comparing two releases is the regression workflow this lab
# exists for, and no release has an admin port yet. But which of the two readiness
# paths a run took decides what its cold-start number means, so it is stated up
# front rather than inferred afterwards from a JSON field.
if [ "$missing" -eq 0 ]; then
  if octo_has_admin_port "$TARGET"; then
    if metrics_wanted "$TARGET"; then
      ok "observe" "probes on :${ADMIN_PORT} and /metrics — readiness via /readyz"
      if metrics_blocks_wanted "$TARGET"; then
        warn "per-block metrics requested (${METRICS_BLOCKS}) — the engine will emit an event around EVERY block, in every flow"
        dim "    this changes what is measured; the run id is stamped -blockmetrics"
      fi
    else
      ok "observe" "probes on :${ADMIN_PORT}, metrics off (METRICS=0) — readiness via /readyz"
    fi
  else
    # preflight does not load a scenario, so the route may not be known here.
    skipped "observe" "this build serves no admin port — readiness falls back to polling ${READY_ROUTE:-${ROUTE:-the scenario route}}"
    dim "    server-side metrics need a build that has it: BUILD=dev"
  fi
fi

# --- what is actually under test ----------------------------------------------
# Printed for every run, because "which build produced this number" is the one
# question a benchmark result has to be able to answer.
if [ "$(build_channel)" = "dev" ]; then
  ok "build" "dev — $(version_under_test "$TARGET")"
  dim "    source  $(dev_build_field source.path "$TARGET")"
  dim "    commit  $(dev_build_field source.shortCommit "$TARGET") on $(dev_build_field source.branch "$TARGET") — $(dev_build_field source.subject "$TARGET")"
  if [ "$(dev_build_field source.dirty "$TARGET")" = "true" ]; then
    warn "the source tree has uncommitted changes; this result is not reproducible from a commit"
  fi
else
  ok "build" "release — $(version_under_test "$TARGET")"
fi

# --- timing helper ------------------------------------------------------------
if [ "$OS" = "Darwin" ]; then
  [ -x /usr/bin/time ] && ok "time" "/usr/bin/time -l (BSD)" || absent "time" "/usr/bin/time not found"
else
  [ -x /usr/bin/time ] && ok "time" "/usr/bin/time -v (GNU)" || absent "time" "/usr/bin/time not found — install the 'time' package"
fi

# --- soft advisories ----------------------------------------------------------
fd="$(ulimit -n)"
if [ "$fd" != "unlimited" ] && [ "$fd" -lt 8192 ] 2>/dev/null; then
  warn "file descriptor limit is $fd; high arrival rates may exhaust it. Try: ulimit -n 65536"
fi

if [ "$(printf '%s' "$BASE_URL" | grep -c 'localhost\|127\.0\.0\.1')" -gt 0 ]; then
  dim "  note: load generator and server share this host — see METHODOLOGY.md caveats"
fi

echo >&2
if [ "$missing" -gt 0 ]; then
  die "$missing requirement(s) missing"
fi
step "preflight ok"
