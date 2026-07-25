#!/usr/bin/env bash
# Stage one rendered variant of a scenario into a clean directory.
#
# Usage: stage-config.sh <scenario-dir> <variant> <dest-dir>
#
# Tunables are per-scenario: TUNABLES in scenario.env names them (default
# "workers buffer pool"). For the tuned variant each knob's value is read from
# TUNED_<UPPERCASE_KNOB> — e.g. TUNED_WORKERS, TUNED_MAXOPENCONNS. Unset knobs
# keep whatever integration.yaml declares.
#
# Three reasons this exists rather than pointing octo at the scenario directly:
#
#   1. `octo --config <dir>` loads EVERY config in the directory, so more than one
#      yaml side by side would all load and collide on the port.
#   2. Both arms are *derived* from the scenario's single integration.yaml —
#      baseline by stripping the tuning knobs, tuned by rewriting them. See
#      lab/bin/render-config.py.
#   3. DEST is always an absolute path. Octo 0.4.2 fails to resolve template
#      resources when --config is given a relative directory ("resource id
#      escapes the resource root"), so the harness never hands it one.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

SCEN_DIR="${1:?usage: stage-config.sh <scenario-dir> <variant> <dest-dir>}"
VARIANT="${2:?variant required (baseline|tuned)}"
DEST="${3:?dest dir required}"

SRC="$SCEN_DIR/octo/integration.yaml"
[ -f "$SRC" ] || die "no integration at $SRC"

rm -rf "$DEST"
mkdir -p "$DEST"
DEST="$(cd "$DEST" && pwd)"

TUNABLES="${TUNABLES:-workers buffer pool}"

render_args=(--tunables "$TUNABLES")
if [ "$VARIANT" = "tuned" ]; then
  for knob in $TUNABLES; do
    # workers -> TUNED_WORKERS, maxOpenConns -> TUNED_MAXOPENCONNS
    var="TUNED_$(printf '%s' "$knob" | tr '[:lower:]' '[:upper:]')"
    val="$(eval "printf '%s' \"\${$var:-}\"")"
    [ -n "$val" ] && render_args+=(--set "$knob=$val")
  done
fi

# ${a[@]+"${a[@]}"} keeps an empty array safe under `set -u` on bash 3.2 (macOS).
python3 "$LAB_BIN/render-config.py" "$SRC" "$VARIANT" \
  ${render_args[@]+"${render_args[@]}"} -o "$DEST/octo.yaml" \
  || die "could not render the $VARIANT variant of $SRC"

# Copy sibling asset directories the config may reference. Resource paths in Octo
# configs resolve relative to the config's own directory.
for asset in templates data static; do
  [ -d "$SCEN_DIR/octo/$asset" ] && cp -R "$SCEN_DIR/octo/$asset" "$DEST/"
done

printf '%s\n' "$DEST"
