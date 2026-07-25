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

step "preflight (host=$HOST_PROFILE target=$TARGET)"

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
      ok "octo" "$(octo_version)  ($octo_path)"
    else
      absent "octo" "not found${OCTO_BIN:+ at OCTO_BIN=$OCTO_BIN} — see https://juancavallotti.github.io/octo/getting-started/installation/"
    fi
    skipped "docker" "not needed for TARGET=native"
    ;;
  docker)
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
      ok "docker" "$(docker info --format '{{.ServerVersion}} cpus={{.NCPU}} mem={{.MemTotal}}' 2>/dev/null)"
      if docker image inspect "$OCTO_IMAGE" >/dev/null 2>&1; then
        ok "image" "$OCTO_IMAGE ($(docker_image_version "$OCTO_IMAGE"))"
      else
        absent "image" "$OCTO_IMAGE not pulled — run: docker pull $OCTO_IMAGE"
      fi
    else
      absent "docker" "daemon not reachable"
    fi
    skipped "octo" "not needed for TARGET=docker"
    ;;
  *) die "unknown TARGET '$TARGET' (expected native or docker)" ;;
esac

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
