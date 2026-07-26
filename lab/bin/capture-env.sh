#!/usr/bin/env bash
# Capture the hardware, OS, and version-under-test fingerprint for a run.
#
# Usage: capture-env.sh <out.json>
#
# Reads TARGET, HOST, SCENARIO_ID from the environment. Branches on uname so the
# same script works on the macOS lab host and on a Linux/GCE runner later.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

OUT="${1:?usage: capture-env.sh <out.json>}"
TARGET="${TARGET:-native}"
load_host "${HOST:-local}"

# ------------------------------------------------------------- hardware ------

case "$OS" in
  Darwin)
    CPU_MODEL="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
    CPU_LOGICAL="$(sysctl -n hw.logicalcpu 2>/dev/null || echo 0)"
    CPU_PHYSICAL="$(sysctl -n hw.physicalcpu 2>/dev/null || echo 0)"
    CPU_PERF="$(sysctl -n hw.perflevel0.logicalcpu 2>/dev/null || echo 0)"
    CPU_EFF="$(sysctl -n hw.perflevel1.logicalcpu 2>/dev/null || echo 0)"
    MEM_BYTES="$(sysctl -n hw.memsize 2>/dev/null || echo 0)"
    OS_NAME="macOS"
    OS_VERSION="$(sw_vers -productVersion 2>/dev/null || echo unknown)"
    OS_BUILD="$(sw_vers -buildVersion 2>/dev/null || echo unknown)"
    ARCH="$(uname -m)"
    CLOUD_MACHINE_TYPE=""
    ;;
  Linux)
    CPU_MODEL="$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)"
    [ -n "$CPU_MODEL" ] || CPU_MODEL="$(awk -F': ' '/^Model/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)"
    CPU_LOGICAL="$(nproc 2>/dev/null || echo 0)"
    CPU_PHYSICAL="$(lscpu -p=Core,Socket 2>/dev/null | grep -vc '^#' || echo 0)"
    CPU_PERF=0
    CPU_EFF=0
    MEM_BYTES="$(awk '/^MemTotal/{print $2*1024; exit}' /proc/meminfo 2>/dev/null || echo 0)"
    OS_NAME="$(. /etc/os-release 2>/dev/null && echo "$NAME" || echo Linux)"
    OS_VERSION="$(. /etc/os-release 2>/dev/null && echo "$VERSION_ID" || uname -r)"
    OS_BUILD="$(uname -r)"
    ARCH="$(uname -m)"
    # GCE metadata server, when present, names the machine type.
    CLOUD_MACHINE_TYPE="$(curl -s --max-time 1 -H 'Metadata-Flavor: Google' \
      http://metadata.google.internal/computeMetadata/v1/instance/machine-type 2>/dev/null | awk -F/ '{print $NF}' || true)"
    ;;
  *)
    die "unsupported OS '$OS'"
    ;;
esac

FD_LIMIT="$(ulimit -n)"

# ------------------------------------------- version under test + envelope ----

OCTO_VERSION=""
OCTO_ARTIFACT=""
OCTO_ARTIFACT_BYTES=0
IMAGE_REF=""
IMAGE_DIGEST=""
DOCKER_NCPU=0
DOCKER_MEM_BYTES=0
DOCKER_SERVER=""

BUILD_CHANNEL="$(build_channel)"
# For a dev build the run is attributable only through the source commit, so the
# provenance record travels with the result rather than living in someone's shell
# history. Empty for BUILD=release, where the version string is the whole story.
BUILD_META="$(BUILD="$BUILD_CHANNEL" "$LAB_BIN/build-octo.sh" "$TARGET" --meta 2>/dev/null || true)"

case "$TARGET" in
  native)
    OCTO_VERSION="$(version_under_test native)"
    OCTO_ARTIFACT="$(octo_bin)"
    if [ -n "$OCTO_ARTIFACT" ] && [ -f "$OCTO_ARTIFACT" ]; then
      OCTO_ARTIFACT_BYTES="$(wc -c < "$OCTO_ARTIFACT" | tr -d ' ')"
    fi
    ;;
  docker)
    IMAGE_REF="$(octo_image)"
    OCTO_VERSION="$(version_under_test docker)"
    IMAGE_DIGEST="$(docker image inspect "$IMAGE_REF" --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
    [ -n "$IMAGE_DIGEST" ] || IMAGE_DIGEST="$(docker image inspect "$IMAGE_REF" --format '{{.Id}}' 2>/dev/null || true)"
    OCTO_ARTIFACT_BYTES="$(docker image inspect "$IMAGE_REF" --format '{{.Size}}' 2>/dev/null || echo 0)"
    OCTO_ARTIFACT="$IMAGE_REF"
    DOCKER_NCPU="$(docker info --format '{{.NCPU}}' 2>/dev/null || echo 0)"
    DOCKER_MEM_BYTES="$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)"
    DOCKER_SERVER="$(docker info --format '{{.ServerVersion}}' 2>/dev/null || echo unknown)"
    ;;
esac

# ------------------------------------------------- observability under test ----
# Whether the runtime was serving probes and metrics, and whether it was asked for
# per-block telemetry. This belongs in the provenance record because the last of the
# three is not free: watching any block makes the engine emit an event around every
# block, so a run carrying it is not measuring the same thing as one that is not.
ADMIN_PORT_AVAILABLE="$(octo_has_admin_port "$TARGET" && echo true || echo false)"
METRICS_ON="$(metrics_wanted "$TARGET" && echo true || echo false)"
READINESS_METHOD="$(readiness_method "$TARGET")"

K6_VERSION="$(k6_version)"
LAB_COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo uncommitted)"
LAB_DIRTY="$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null | head -1 >/dev/null && \
             [ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ] && echo true || echo false)"

# ------------------------------------------------------------------ emit ------

export CPU_MODEL CPU_LOGICAL CPU_PHYSICAL CPU_PERF CPU_EFF MEM_BYTES \
       OS_NAME OS_VERSION OS_BUILD ARCH FD_LIMIT CLOUD_MACHINE_TYPE \
       OCTO_VERSION OCTO_ARTIFACT OCTO_ARTIFACT_BYTES IMAGE_REF IMAGE_DIGEST \
       DOCKER_NCPU DOCKER_MEM_BYTES DOCKER_SERVER K6_VERSION LAB_COMMIT LAB_DIRTY \
       TARGET OUT BUILD_CHANNEL BUILD_META \
       ADMIN_PORT_AVAILABLE METRICS_ON READINESS_METHOD

mkdir -p "$(dirname "$OUT")"
python3 - <<'PY'
import json, os, subprocess

def i(k, d=0):
    try: return int(os.environ.get(k, d) or d)
    except ValueError: return d

target = os.environ["TARGET"]

env = {
    "capturedAt": subprocess.run(["date","-u","+%Y-%m-%dT%H:%M:%SZ"],
                                 capture_output=True, text=True).stdout.strip(),
    "hostProfile": os.environ.get("HOST_PROFILE", "unknown"),
    "target": target,
    "scenario": os.environ.get("SCENARIO_ID", ""),
    "versionUnderTest": os.environ.get("OCTO_VERSION", "unknown"),
    # release = the published distribution; dev = built here from source.
    "buildChannel": os.environ.get("BUILD_CHANNEL", "release"),
    "hardware": {
        "cpuModel":     os.environ.get("CPU_MODEL", "unknown"),
        "arch":         os.environ.get("ARCH", "unknown"),
        "logicalCores": i("CPU_LOGICAL"),
        "physicalCores":i("CPU_PHYSICAL"),
        "performanceCores": i("CPU_PERF") or None,
        "efficiencyCores":  i("CPU_EFF") or None,
        "memoryBytes":  i("MEM_BYTES"),
        "cloudMachineType": os.environ.get("CLOUD_MACHINE_TYPE") or None,
    },
    "os": {
        "name":    os.environ.get("OS_NAME", "unknown"),
        "version": os.environ.get("OS_VERSION", "unknown"),
        "build":   os.environ.get("OS_BUILD", "unknown"),
        "fileDescriptorLimit": os.environ.get("FD_LIMIT", "unknown"),
    },
    "runtime": {
        "artifact":      os.environ.get("OCTO_ARTIFACT", ""),
        "artifactBytes": i("OCTO_ARTIFACT_BYTES"),
    },
    "tooling": {
        "k6":        os.environ.get("K6_VERSION", "unknown"),
        "labCommit": os.environ.get("LAB_COMMIT", "unknown"),
        "labDirty":  os.environ.get("LAB_DIRTY", "false") == "true",
    },
    # What the runtime was asked to expose about itself, and how the harness decided
    # it was up. Recorded on every run: "ready" means a later moment to a build with
    # no admin port than to one with it, so a cold-start figure is only comparable
    # against another captured the same way.
    "observability": {
        "adminPortAvailable": os.environ.get("ADMIN_PORT_AVAILABLE") == "true",
        "adminBaseUrl": os.environ.get("ADMIN_BASE_URL", ""),
        "metrics": os.environ.get("METRICS_ON") == "true",
        # Per-block telemetry costs an event around every block in every flow, so a
        # run that carries it is stamped -blockmetrics in its id as well.
        "metricsBlocks": os.environ.get("METRICS_BLOCKS") or None,
        "readinessMethod": os.environ.get("READINESS_METHOD", "route"),
    },
    "loadGenerator": {
        "baseUrl": os.environ.get("BASE_URL", ""),
        # True when k6 and the server share a host: contention is a known limitation.
        "colocatedWithServer": any(h in os.environ.get("BASE_URL", "")
                                   for h in ("localhost", "127.0.0.1")),
    },
}

# The source a dev build came from. Without this a "0.4.3-dev.dd065f8" number is
# a string nobody can trace: which checkout, which branch, was the tree dirty.
raw_meta = os.environ.get("BUILD_META", "").strip()
if raw_meta:
    try:
        meta = json.loads(raw_meta)
    except json.JSONDecodeError:
        meta = None
    if meta:
        env["build"] = {
            "channel":    meta.get("channel"),
            "buildId":    meta.get("buildId"),
            "version":    meta.get("version"),
            "source":     meta.get("source"),
            "goVersion":  meta.get("goVersion"),
            "buildTags":  meta.get("buildTags"),
            "builtAt":    meta.get("builtAt"),
        }

if target == "docker":
    env["container"] = {
        "image":         os.environ.get("IMAGE_REF", ""),
        "digest":        os.environ.get("IMAGE_DIGEST", ""),
        "imageBytes":    i("OCTO_ARTIFACT_BYTES"),
        "dockerServer":  os.environ.get("DOCKER_SERVER", ""),
        # The envelope the container runtime was actually given — usually smaller
        # than the host, which is why cross-target comparison is only indicative.
        "allocatedCpus":        i("DOCKER_NCPU"),
        "allocatedMemoryBytes": i("DOCKER_MEM_BYTES"),
        # The deliberate cap, when one was requested — present only for runs whose
        # point is to describe a known deployment size.
        "cpuLimit":  os.environ.get("CPU_LIMIT") or None,
        "memLimit":  os.environ.get("MEM_LIMIT") or None,
    }

with open(os.environ["OUT"], "w") as f:
    json.dump(env, f, indent=2)
    f.write("\n")
PY

dim "  env -> $OUT"
