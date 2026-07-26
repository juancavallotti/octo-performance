#!/usr/bin/env bash
# Build the runtime from a source checkout so unreleased work can be benchmarked
# against the released distribution on the same host.
#
# Usage:
#   build-octo.sh native            build if needed; print the binary path
#   build-octo.sh docker            build if needed; print the image ref
#   build-octo.sh <target> --resolve   print the artifact only if it already exists
#   build-octo.sh <target> --meta      print the build's provenance JSON
#
# Environment:
#   OCTO_SRC   the octo checkout (default: a sibling directory named "octo")
#
# Artifacts land under .stage/builds/<key>/ where <key> is the source commit, so
# two commits never overwrite each other and a rebuilt commit is free.
#
# Why the native build carries no build tags while the container build carries
# `k8s`: those are the two artifacts that actually ship. The released standalone
# distribution is the untagged binary; the published runtime image is built from
# runtime/Dockerfile, whose GOTAGS defaults to k8s. Building the dev artifact any
# other way would compare a binary against a binary that nobody runs, and the
# services provider compiled in is not a detail — it decides which queue and
# leader-election implementations the flow actually uses.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

TARGET="${1:?usage: build-octo.sh native|docker [--resolve|--meta]}"
MODE="${2:-build}"

SRC="${OCTO_SRC:-$REPO_ROOT/../octo}"

# ------------------------------------------------------------------- source ---

resolve_src() {
  [ -d "$SRC" ] || die "no octo source at $SRC (set OCTO_SRC)"
  SRC="$(cd "$SRC" && pwd)"
  [ -d "$SRC/.git" ] || die "$SRC is not a git checkout — provenance would be unrecordable"
  [ -d "$SRC/runtime/octo" ] || die "$SRC does not look like the octo repo (no runtime/octo)"
}

# The identity of the tree being built. A dirty tree gets its own key so an
# uncommitted experiment never lands in the same directory as the commit it is
# based on — and carries `.dirty` all the way into the run id, because a result
# from a tree nobody else can check out is not attributable.
src_commit() { git -C "$SRC" rev-parse --short=7 HEAD 2>/dev/null || echo unknown; }
src_dirty()  { [ -n "$(git -C "$SRC" status --porcelain 2>/dev/null)" ] && echo true || echo false; }

build_key() {
  local c d; c="$(src_commit)"; d="$(src_dirty)"
  [ "$d" = true ] && printf '%s-dirty\n' "$c" || printf '%s\n' "$c"
}

# ------------------------------------------------------------------ artifacts --

BUILD_DIR=""
init_paths() {
  resolve_src
  BUILD_DIR="$REPO_ROOT/.stage/builds/$(build_key)"
  NATIVE_BIN="$BUILD_DIR/octo"
  IMAGE_REF="octo-runtime:dev-$(build_key)"
  META="$BUILD_DIR/build-${TARGET}.json"
}

artifact_ref() {
  case "$TARGET" in
    native) printf '%s\n' "$NATIVE_BIN" ;;
    docker) printf '%s\n' "$IMAGE_REF" ;;
    *) die "unknown target '$TARGET' (expected native or docker)" ;;
  esac
}

artifact_exists() {
  case "$TARGET" in
    native) [ -x "$NATIVE_BIN" ] ;;
    docker) docker image inspect "$IMAGE_REF" >/dev/null 2>&1 ;;
  esac
}

# ---------------------------------------------------------------- provenance ---

write_meta() {
  local version="$1" artifact="$2" bytes="$3" tags="$4"
  mkdir -p "$BUILD_DIR"
  OCTO_SRC_PATH="$SRC" \
  B_COMMIT="$(git -C "$SRC" rev-parse HEAD 2>/dev/null || echo unknown)" \
  B_SHORT="$(src_commit)" \
  B_BRANCH="$(git -C "$SRC" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)" \
  B_DIRTY="$(src_dirty)" \
  B_DESCRIBE="$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo unknown)" \
  B_SUBJECT="$(git -C "$SRC" log -1 --pretty=%s 2>/dev/null || echo '')" \
  B_COMMITTED="$(git -C "$SRC" log -1 --pretty=%cI 2>/dev/null || echo '')" \
  B_VERSION="$version" B_ARTIFACT="$artifact" B_BYTES="$bytes" B_TAGS="$tags" \
  B_TARGET="$TARGET" B_GO="$(go version 2>/dev/null | awk '{print $3}' || echo unknown)" \
  B_AT="$(now_utc)" B_OUT="$META" \
  python3 - <<'PY'
import json, os

short = os.environ["B_SHORT"]
dirty = os.environ["B_DIRTY"] == "true"
version = os.environ["B_VERSION"] or "unknown"

# The whole point of this file. The source declares the same version constant as
# the last release, so a dev build and the release it is based on are
# indistinguishable by `octo version` alone — same string, same run id, same row
# in the regression table. Appending the commit is what keeps them apart.
build_id = f"{version}-dev.{short}" + (".dirty" if dirty else "")

meta = {
    "channel": "dev",
    "target": os.environ["B_TARGET"],
    "buildId": build_id,
    "version": version,
    "source": {
        "path": os.environ["OCTO_SRC_PATH"],
        "commit": os.environ["B_COMMIT"],
        "shortCommit": short,
        "branch": os.environ["B_BRANCH"],
        "describe": os.environ["B_DESCRIBE"],
        "subject": os.environ["B_SUBJECT"],
        "committedAt": os.environ["B_COMMITTED"] or None,
        "dirty": dirty,
    },
    "artifact": os.environ["B_ARTIFACT"],
    "artifactBytes": int(os.environ.get("B_BYTES") or 0),
    "goVersion": os.environ["B_GO"],
    "buildTags": os.environ["B_TAGS"],
    "builtAt": os.environ["B_AT"],
}
with open(os.environ["B_OUT"], "w") as fh:
    json.dump(meta, fh, indent=2)
    fh.write("\n")
PY
}

# --------------------------------------------------------------------- build ---

build_native() {
  mkdir -p "$BUILD_DIR"
  require go "Install Go, or benchmark the released distribution with BUILD=release."

  step "building octo from source ($(build_key))"
  dim "  $SRC -> $NATIVE_BIN"

  # Untagged: this is the standalone distribution's shape. See the header.
  # Go's own build cache makes an unchanged rebuild take a fraction of a second,
  # so a dirty tree is rebuilt every time rather than guessed at.
  ( cd "$SRC" && go build \
      -ldflags "-X main.BuildDate=$(now_utc)" \
      -o "$NATIVE_BIN" ./runtime/octo ) >&2 \
    || die "go build failed in $SRC"

  local version bytes
  version="$("$NATIVE_BIN" version 2>/dev/null | awk '{print $2; exit}' | tr -d '\r')"
  bytes="$(wc -c < "$NATIVE_BIN" | tr -d ' ')"
  write_meta "$version" "$NATIVE_BIN" "$bytes" ""
  dim "  built $version ($(( bytes / 1048576 )) MiB)"
}

build_docker() {
  mkdir -p "$BUILD_DIR"
  require docker "Install Docker, or use TARGET=native."
  [ -f "$SRC/runtime/Dockerfile" ] || die "no runtime/Dockerfile in $SRC"

  step "building octo runtime image from source ($(build_key))"
  dim "  $SRC -> $IMAGE_REF"

  # Build context is the repo root: the Go module is rooted there.
  # GOTAGS is left at the Dockerfile's default (k8s) so this image matches the
  # published one in the way that matters — same services provider compiled in.
  docker build -t "$IMAGE_REF" -f "$SRC/runtime/Dockerfile" "$SRC" >&2 \
    || die "docker build failed"

  local version bytes
  version="$(docker_image_version "$IMAGE_REF")"
  bytes="$(docker image inspect "$IMAGE_REF" --format '{{.Size}}' 2>/dev/null || echo 0)"
  write_meta "$version" "$IMAGE_REF" "$bytes" "k8s"
  dim "  built $version ($(( bytes / 1048576 )) MiB)"
}

# A clean tree is cached by commit: the artifact cannot have changed. A dirty
# tree is rebuilt unconditionally, because the only honest assumption about
# uncommitted work is that it moved.
needs_build() {
  artifact_exists || return 0
  [ -f "$META" ] || return 0
  [ "$(src_dirty)" = true ] && return 0
  return 1
}

# ---------------------------------------------------------------------- main ---

init_paths

case "$MODE" in
  --resolve)
    artifact_exists || exit 1
    artifact_ref
    ;;
  --meta)
    [ -f "$META" ] || die "no build recorded for $TARGET at $BUILD_DIR — run: task build"
    cat "$META"
    ;;
  build)
    if needs_build; then
      case "$TARGET" in
        native) build_native ;;
        docker) build_docker ;;
        *) die "unknown target '$TARGET'" ;;
      esac
    else
      dim "  reusing $(artifact_ref) (source clean at $(build_key))"
    fi
    artifact_ref
    ;;
  *) die "unknown mode '$MODE'" ;;
esac
